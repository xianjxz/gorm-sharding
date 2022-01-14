package gormsharding

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/longbridgeapp/sqlparser"
	"gorm.io/gorm"
)

var (
	ErrMissingShardingKey = errors.New("sharding key or id required, and use operator =")
	ErrInvalidID          = errors.New("invalid id format")
)

type Sharding struct {
	*gorm.DB
	ConnPool  *ConnPool
	Resolvers map[string]Resolver

	querys sync.Map
}

type Resolver struct {
	// EnableFullTable whether to enable a full table
	EnableFullTable bool
	// ShardingColumn column name of the sharding column, for example: user_id
	ShardingColumn string
	// ShardingAlgorithm implement the sharding algorithm for generate table name suffix
	ShardingAlgorithm func(columnValue interface{}) (suffix string, err error)
	// ShardingAlgorithm sharding algorithm by primary key, if it posible.
	ShardingAlgorithmByPrimaryKey func(id int64) (suffix string)
	// PrimaryKeyGenerate for generate primary key
	PrimaryKeyGenerate func(tableIdx int64) int64
}

func Register(resolvers map[string]Resolver) Sharding {
	return Sharding{Resolvers: resolvers}
}

// Name plugin name for Gorm plugin interface
func (s *Sharding) Name() string {
	return "gorm:sharding"
}

// LastQuery get last SQL query
func (s *Sharding) LastQuery() string {
	if query, ok := s.querys.Load("last_query"); ok {
		return query.(string)
	}

	return ""
}

// Initialize implement for Gorm plugin interface
func (s *Sharding) Initialize(db *gorm.DB) error {
	s.DB = db
	s.registerConnPool(db)
	return nil
}

// resolve split the old query to full table query and sharding table query
func (s *Sharding) resolve(query string, args ...interface{}) (ftQuery, stQuery, tableName string, err error) {
	ftQuery = query
	stQuery = query
	if len(s.Resolvers) == 0 {
		return
	}

	expr, err := sqlparser.NewParser(strings.NewReader(query)).ParseStatement()
	if err != nil {
		return ftQuery, stQuery, tableName, nil
	}

	var table *sqlparser.TableName
	var condition sqlparser.Expr
	var isInsert bool
	var insertNames []*sqlparser.Ident
	var insertValues []sqlparser.Expr

	switch stmt := expr.(type) {
	case *sqlparser.SelectStatement:
		tbl, ok := stmt.FromItems.(*sqlparser.TableName)
		if !ok {
			return
		}
		if stmt.Hint != nil && stmt.Hint.Value == "nosharding" {
			return
		}
		table = tbl
		condition = stmt.Condition

	case *sqlparser.InsertStatement:
		table = stmt.TableName
		isInsert = true
		insertNames = stmt.ColumnNames
		insertValues = stmt.Expressions[0].Exprs
	case *sqlparser.UpdateStatement:
		condition = stmt.Condition
		table = stmt.TableName
	case *sqlparser.DeleteStatement:
		condition = stmt.Condition
		table = stmt.TableName
	default:
		return ftQuery, stQuery, "", sqlparser.ErrNotImplemented
	}

	tableName = table.Name.Name
	r, ok := s.Resolvers[tableName]
	if !ok {
		return
	}

	var value interface{}
	var isID bool
	if isInsert {
		value, isID, err = s.insertValue(r.ShardingColumn, insertNames, insertValues, args...)
		if err != nil {
			return
		}
	} else {
		value, isID, err = s.nonInsertValue(r.ShardingColumn, condition, args...)
		if err != nil {
			return
		}
	}

	var suffix string

	if isID {
		if id, ok := value.(int64); ok {
			suffix = r.ShardingAlgorithmByPrimaryKey(id)
		} else if idUint, ok := value.(uint64); ok {
			suffix = r.ShardingAlgorithmByPrimaryKey(int64(idUint))
		} else if idStr, ok := value.(string); ok {
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				return ftQuery, stQuery, tableName, ErrInvalidID
			}
			suffix = r.ShardingAlgorithmByPrimaryKey(id)
		} else {
			return ftQuery, stQuery, tableName, ErrInvalidID
		}
	} else {
		suffix, err = r.ShardingAlgorithm(value)
		if err != nil {
			return
		}
	}

	newTable := &sqlparser.TableName{Name: &sqlparser.Ident{Name: tableName + suffix}}

	fillID := true
	if isInsert {
		for _, name := range insertNames {
			if name.Name == "id" {
				fillID = false
				break
			}
		}
		if fillID {
			tblIdx, err := strconv.Atoi(strings.Replace(suffix, "_", "", 1))
			if err != nil {
				return ftQuery, stQuery, tableName, err
			}
			id := r.PrimaryKeyGenerate(int64(tblIdx))
			insertNames = append(insertNames, &sqlparser.Ident{Name: "id"})
			insertValues = append(insertValues, &sqlparser.NumberLit{Value: strconv.FormatInt(id, 10)})
		}
	}

	switch stmt := expr.(type) {
	case *sqlparser.InsertStatement:
		if fillID {
			stmt.ColumnNames = insertNames
			stmt.Expressions[0].Exprs = insertValues
		}
		ftQuery = stmt.String()
		stmt.TableName = newTable
		stQuery = stmt.String()
	case *sqlparser.SelectStatement:
		ftQuery = stmt.String()
		stmt.FromItems = newTable
		stmt.OrderBy = replaceOrderByTableName(stmt.OrderBy, tableName, newTable.Name.Name)
		stQuery = stmt.String()
	case *sqlparser.UpdateStatement:
		ftQuery = stmt.String()
		stmt.TableName = newTable
		stQuery = stmt.String()
	case *sqlparser.DeleteStatement:
		ftQuery = stmt.String()
		stmt.TableName = newTable
		stQuery = stmt.String()
	}

	return
}

func (s *Sharding) insertValue(key string, names []*sqlparser.Ident, exprs []sqlparser.Expr, args ...interface{}) (value interface{}, isID bool, err error) {
	bind := false
	find := false

	if len(names) != len(exprs) {
		return nil, false, errors.New("column names and expressions mismatch")
	}

	for i, name := range names {
		if name.Name == key {
			switch expr := exprs[i].(type) {
			case *sqlparser.BindExpr:
				bind = true
				value = expr.Name
			case *sqlparser.StringLit:
				value = expr.Value
			case *sqlparser.NumberLit:
				value = expr.Value
			default:
				return nil, false, sqlparser.ErrNotImplemented
			}
			find = true
			break
		}
	}
	if !find {
		return nil, false, ErrMissingShardingKey
	}

	if bind {
		value, err = getBindValue(value, args)
	}

	return
}

func (s *Sharding) nonInsertValue(key string, condition sqlparser.Expr, args ...interface{}) (value interface{}, isID bool, err error) {
	bind := false
	find := false

	err = sqlparser.Walk(sqlparser.VisitFunc(func(node sqlparser.Node) error {
		if n, ok := node.(*sqlparser.BinaryExpr); ok {
			if x, ok := n.X.(*sqlparser.Ident); ok {
				if x.Name == key && n.Op == sqlparser.EQ {
					find = true
					isID = false
					bind = false
					switch expr := n.Y.(type) {
					case *sqlparser.BindExpr:
						bind = true
						value = expr.Name
					case *sqlparser.StringLit:
						value = expr.Value
					case *sqlparser.NumberLit:
						value = expr.Value
					default:
						return sqlparser.ErrNotImplemented
					}
					return nil
				} else if x.Name == "id" && n.Op == sqlparser.EQ {
					find = true
					isID = true
					bind = false
					switch expr := n.Y.(type) {
					case *sqlparser.BindExpr:
						bind = true
						value = expr.Name
					case *sqlparser.NumberLit:
						value = expr.Value
					default:
						return ErrInvalidID
					}
					return nil
				}
			}
		}
		return nil
	}), condition)
	if err != nil {
		return
	}

	if !find {
		return nil, false, ErrMissingShardingKey
	}

	if bind {
		value, err = getBindValue(value, args)
	}

	return
}

func replaceOrderByTableName(orderBy []*sqlparser.OrderingTerm, oldName, newName string) []*sqlparser.OrderingTerm {
	for i, term := range orderBy {
		if x, ok := term.X.(*sqlparser.QualifiedRef); ok {
			if x.Table.Name == oldName {
				x.Table.Name = newName
				orderBy[i].X = x
			}
		}
	}

	return orderBy
}

func getBindValue(value interface{}, args []interface{}) (interface{}, error) {
	bindPos := strings.Replace(value.(string), "$", "", 1)
	pos, err := strconv.Atoi(bindPos)
	if err != nil {
		return nil, err
	}
	return args[pos-1], nil
}