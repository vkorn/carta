package carta

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/golang/protobuf/ptypes/timestamp"
)

// Representation of SQL response mapping to a proto response message
// protoc-gen-map generates service server methods which implement interfaces generated by protoc-gen-go
type Mapperr struct {
	SqlMap *SqlMap //Mapping of top level element, must be a pointer to a struct or pointer to a slice/array
	Logs   map[string]bool
	Error  error

	columns     map[string]int    // columns returned by executing the sql, mapped to their index
	columnTypes []*sql.ColumnType // column types returned by executing the SQL
}

// SqlMap represents the schema of our response which is infered from the reveived struct
// stores types of proto messages and their corresponding column name
// if the proto message has repeated and/or nested fields, they are recursively stores in
// Associations and Collections
type SqlMap struct {
	Name string      // Name of the field
	Crd  Cardinality // this field is empty for sqlMap of the top level element, only has-many and has-one relationships have this set

	MapType reflect.Type

	// Columns of the SQL response which are present in this struct
	PresentColumns map[string]*ColumnField

	// Columns of all parents structs, used to detect whether a new struct should be appended for has-many relationships
	AncestorColumns map[string]*ColumnField

	// Nested structs
	SubMaps map[int]*SqlMap // int is the ith element of this struct where the submap exists

	Error error    // breaking issue
	Logs  []string // non-breaking issue

	mapSetter mapSetter
}

// Maps db rows onto the complex struct,
// Response must be a struct, pointer to a struct for our response, a slice of structs or slice of pointers to a struct
func Map(rows *sql.Rows, dst interface{}) error {
	var (
		mapper *Mapper
		err    error
	)
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	dstTyp := reflect.TypeOf(dst)
	mapper, ok := mapperCache.loadMap(columns, dstTyp)
	if ok {
		return mapper.loadRows(rows, dst)
	} else {
		if isSlicePtr(dstTyp) || isStructPtr(dstTyp) {
			return fmt.Errorf("carta: cannot map rows onto %T, destination must be pointer to a slice(*[]) or pointer to struct", dstTyp)
		}

		// generate new mapper
		if mapper, err = newMapper(dstTyp, nil); err != nil {
			return err
		}

		// Allocate columns
		columnsByName := map[string]column{}
		for i, columnName := range columns {
			columnsByName[columnName] = column{columnTypes[i], i}
		}
		if err = allocateColumns(mapper, columnsByName); err != nil {
			return err
		}
		mapperCache.storeMap(columns, dstTyp, mapper)
	}
	return mapper.loadRows(rows, dst)
}

// Generates SQL mapping recursively according to the the struct and its submessages
// This is done once, after the first sql response is retrieved
func generateSqlMap(sqlMap *SqlMap, columns []string, parent *SqlMap) {
	presentColumns := map[string]*ColumnField{}
	ancestorColumns := map[string]*ColumnField{}
	subMaps := map[int]*SqlMap{}
	containsAllowed := false

	for i := 0; i < sqlMap.MapType.NumField(); i++ {
		field := sqlMap.MapType.Field(i)
		if isAlowedType(field) == true {
			containsAllowed = true
			for j, c := range columns {
				if _, ok := possibleFieldNames(field, parent.Name)[c]; ok {
					presentColumns[c] = &ColumnField{
						field:       &field,
						fieldIndex:  i,
						columnIndex: j,
					}
					// remove claimed column, must preserve order
					columns[j] = ""
					break
				}
			}
		} else if fieldCardinality, isSubMap, err := isSubMap(field); isSubMap == true {
			if err != nil {
				sqlMap.Error = err
				return
			}
			subMap := SqlMap{
				Name:    field.Name,
				MapType: field.Type,
				Crd:     fieldCardinality,
			}
			switch fieldCardinality {
			case Association:
				subMap.mapSetter = newStructSetter(field.Type)
			case Collection:
				subMap.mapSetter = newListSetter(field.Type)
			}
			subMaps[i] = &subMap
			generateSqlMap(&subMap, columns, sqlMap)
		}
	}

	if containsAllowed == false {
		sqlMap.Logs = append(sqlMap.Logs, fmt.Sprintf("carta: No allowed fileds were found in %T. At least one exported primative, timestamp.Timestamp, or sql.NullXXX field must be present in the type.", sqlMap.MapType))
		//delete all subMaps, it is impossible to resolve any submaps if no primative/timestamp/sql.NullXXX exists in the parent struct
		sqlMap.SubMaps = map[int]*SqlMap{}
		return
	} else if len(presentColumns) == 0 {
		sqlMap.Logs = append(sqlMap.Logs, fmt.Sprintf("carta: no corresponging columns were were found in %T. Struct will be omitted.", sqlMap.MapType))
		return
	}

	sqlMap.PresentColumns = presentColumns

	for columnName, columnField := range presentColumns {
		ancestorColumns[columnName] = columnField
	}
	for columnName, columnField := range sqlMap.AncestorColumns {
		ancestorColumns[columnName] = columnField
	}

	for _, subMap := range subMaps {
		subMap.AncestorColumns = ancestorColumns
		generateSqlMap(subMap, columns, sqlMap)
	}

	return
}

// test wether incoming type is a pointer to a struct, courtesy of BQ api
func isStructPtr(t reflect.Type) bool {
	return t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Struct
}
func isSlicePtr(t reflect.Type) bool {
	return t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Slice
}

// Primitive types generated by protoc-gen-go
var allowedKinds = map[reflect.Kind]bool{
	reflect.Float64: true,
	reflect.Float32: true,
	reflect.Int32:   true,
	reflect.Uint32:  true,
	reflect.Int64:   true,
	reflect.Uint64:  true,
	reflect.Bool:    true,
	reflect.String:  true,
}

// Allowed non-primitives
var allowedTypes = map[reflect.Type]bool{
	reflect.TypeOf(&timestamp.Timestamp{}): true,
}

func isAlowedType(field reflect.StructField) bool {
	kind := field.Type.Kind()
	if allowedKinds[kind] == true {
		return true
	} else if kind == reflect.Ptr && allowedTypes[field.Type] {
		return true
	} else if isEnum(field) {
		return true
	} else {
		return false
	}
}

func isEnum(field reflect.StructField) bool {
	if field.Type.Kind() == reflect.Int32 && field.Type.Name() != "int32" {
		return true
	}
	return false
}

// Begin mapping the proto response
// This is method is called for SQL query response
func (m *Mapper) MapResponse(respMap *ResponseMapping) error {
	var topLvlElem interface{}
	for _, rowValues := range respMap.values {
		m.prepareProtoValues(rowValues, m.SqlMap, respMap.sqlMapVals)
		if respMap.sqlMapVals.IsNill {
			continue
		}
		existingProtoMsg, uniqueId, isUnique := m.findUniqueResp(m.SqlMap, respMap.sqlMapVals, "")
		if isUnique {
			topLvlElem = reflect.New(m.SqlMap.MapType.Elem()).Interface()
			respMap.Responses = append(respMap.Responses, topLvlElem)
		} else {
			topLvlElem = existingProtoMsg
		}
		m.MapRow(rowValues, m.SqlMap, respMap.sqlMapVals, topLvlElem, uniqueId)
		if m.Error != nil {
			return m.Error
		}
	}
	return nil
}

// Map a single row of the sql query
// This function starts with the top level element as input parameter,
// and is called recursively for each Association and Collection on the same row
func (m *Mapper) MapRow(rowValues []interface{}, sqlMap *SqlMap, sqlMapVals *SqlMapVals, protoMsg interface{}, uniqueId string) {
	if m.Error != nil {
		return
	}
	var (
		respValue       reflect.Value
		associationElem interface{}
		associationVals *SqlMapVals
		collectionElem  interface{}
		collectionVals  *SqlMapVals
	)
	respValue = reflect.ValueOf(protoMsg).Elem()
	if _, ok := sqlMapVals.UniqueIds[uniqueId]; ok != true {
		sqlMapVals.UniqueIds[uniqueId] = protoMsg
		for i, column := range sqlMap.PresentColumns {
			protoIndex := sqlMap.Columns[column].index
			respField := respValue.Field(protoIndex)
			if err := setProto(respField, sqlMapVals.ProtoValues[i]); err != nil {
				m.Error = errors.New(fmt.Sprintf("protoc-gen-map: error setting %s with "+column+
					" column value; "+err.Error(), respValue.Type()))
				return
			}
		}
	}
	for mapName, association := range sqlMap.Associations {
		associationVals = sqlMapVals.Associations[mapName]
		m.prepareProtoValues(rowValues, association, associationVals)
		if associationVals.IsNill {
			continue
		}
		existingProtoMsg, subMapUniqueId, isUnique := m.findUniqueResp(association, associationVals, uniqueId)
		if isUnique {
			newAssociationElem := reflect.New(association.MapType.Elem())
			if respValue.Field(association.ParentFieldId).IsNil() == false {
				m.Logs[fmt.Sprintf("%s is defined as a nested field, yet it appearers more than once in mapping, "+
					"consider changing its type to nested and repeated field or review your query or schema.",
					association.Name)] = true
			}
			respValue.Field(association.ParentFieldId).Set(newAssociationElem)
			associationElem = newAssociationElem.Interface()
		} else {
			associationElem = existingProtoMsg
		}
		m.MapRow(rowValues, association, sqlMapVals.Associations[mapName], associationElem, subMapUniqueId)
	}
	for mapName, collection := range sqlMap.Collections {
		collectionVals = sqlMapVals.Collections[mapName]
		m.prepareProtoValues(rowValues, collection, collectionVals)
		if collectionVals.IsNill {
			continue
		}
		existingProtoMsg, subMapUniqueId, isUnique := m.findUniqueResp(collection, collectionVals, uniqueId)
		if isUnique {
			newCollectionElem := reflect.New(collection.SliceElem.Elem())
			respValueField := respValue.Field(collection.ParentFieldId)
			respValueField.Set(
				reflect.Append(
					respValueField,
					newCollectionElem,
				),
			)
			collectionElem = newCollectionElem.Interface()
		} else {
			collectionElem = existingProtoMsg
		}
		m.MapRow(rowValues, collection, sqlMapVals.Collections[mapName], collectionElem, subMapUniqueId)
	}
	return
}

// Preparing copies values from SQL response to SqlMapVals,
// But only those values that belong to the proto message represented in SqlMap
//
// if all values are nil, mapping of the row is skipped,
// this often happens with outer joins
func (m *Mapper) prepareProtoValues(rowValues []interface{}, sqlMap *SqlMap, sqlMapVals *SqlMapVals) {
	if sqlMap.PresentColumns == nil {
		sqlMap.PresentColumns = []string{}
		for name, _ := range m.columns {
			if _, ok := sqlMap.Columns[name]; ok != false {
				sqlMap.PresentColumns = append(sqlMap.PresentColumns, string(name))
			}
		}
	}
	sqlMapVals.ProtoValues = make([]interface{}, len(sqlMap.PresentColumns))
	for i, column := range sqlMap.PresentColumns {
		sqlMapVals.ProtoValues[i] = rowValues[m.columns[column]]
	}
	// if all values are nil, skip
	sqlMapVals.IsNill = true
	for _, val := range sqlMapVals.ProtoValues {
		if val != nil {
			sqlMapVals.IsNill = false
			break
		}
	}
}

// Finds if the unique id for particular sql map values has been processed before.
// note that the uniqueId is a function of current proto values and the parent of the object
// TODO: Implement a better hashing function
func (m *Mapper) findUniqueResp(sqlMap *SqlMap, sqlMapVals *SqlMapVals, parentId string) (protoMsg interface{}, uniqueId string, isUnique bool) {
	uniqueId = parentId + getUniqueId(sqlMapVals.ProtoValues...)
	protoMsg, found := sqlMapVals.UniqueIds[uniqueId]
	isUnique = !found
	return
}

// Generates Response Mapping, an object which stores mapped SQL response values
func (m *Mapper) NewResponseMapping() *ResponseMapping {
	respMap := ResponseMapping{}
	respMap.sqlMapVals = new(SqlMapVals)
	newSubMapVals(m.SqlMap, respMap.sqlMapVals)
	return &respMap
}

func possibleFieldNames(field reflect.StructField, parentName string) map[string]bool {
	nameFromTag := getNameFromTag(field.Tag.Get("db"))
	possibleNames := map[string]bool{
		field.Name:                   true, // Go Field Name
		nameFromTag:                  true, // Proto Field Name
		strings.ToLower(nameFromTag): true,
		strings.ToLower(field.Name):  true,
	}
	return possibleNames
}

// Recursively generates SubMapVals According to the Proto Message Sql Mapping
func newSubMapVals(sqlMap *SqlMap, sqlMapVals *SqlMapVals) {
	sqlMapVals.Associations = make(map[string]*SqlMapVals)
	sqlMapVals.Collections = make(map[mapName]*SqlMapVals)
	sqlMapVals.UniqueIds = make(map[string]interface{})

	for mapName, association := range sqlMap.Associations {
		sqlMapVals.Associations[mapName] = new(SqlMapVals)
		newSubMapVals(association, sqlMapVals.Associations[mapName])
	}
	for mapName, collection := range sqlMap.Collections {
		sqlMapVals.Collections[mapName] = new(SqlMapVals)
		newSubMapVals(collection, sqlMapVals.Collections[mapName])
	}
}

func RegisterEnums(enums map[string]map[string]int32) {
	if EnumVals == nil {
		EnumVals = enums
	} else {
		for enumMapName, enumMapVals := range enums {
			EnumVals[enumMapName] = enumMapVals
		}
	}
}

//If non-breaking issues are found while generating sqlmap, this function prints them
func logSqlMap(sqlm *SqlMap) {
	if len(sqlm.Logs) != 0 {
		for _, message := range sqlm.Logs {
			log.Println("carta: " + message)
		}
	}
	for _, associationSubMap := range sqlm.Associations {
		logSqlMap(associationSubMap)
	}
	for _, collectionSubMap := range sqlm.Collections {
		logSqlMap(collectionSubMap)
	}

}

//If non-breaking issues are found while mapping, this function prints them
func (m *Mapper) Log() {
	if len(m.Logs) != 0 {
		for message, _ := range m.Logs {
			log.Println("carta: " + message)
			delete(m.Logs, message)
		}
	}
}
