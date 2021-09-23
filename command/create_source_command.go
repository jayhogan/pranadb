package command

import (
	"fmt"
	"sync"

	"github.com/alecthomas/repr"
	"github.com/squareup/pranadb/command/parser"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/meta"
	"github.com/squareup/pranadb/perrors"
	"github.com/squareup/pranadb/push/source"
)

type CreateSourceCommand struct {
	lock           sync.Mutex
	e              *Executor
	schemaName     string
	sql            string
	tableSequences []uint64
	ast            *parser.CreateSource
	sourceInfo     *common.SourceInfo
	source         *source.Source
}

func (c *CreateSourceCommand) CommandType() DDLCommandType {
	return DDLCommandTypeCreateSource
}

func (c *CreateSourceCommand) SchemaName() string {
	return c.schemaName
}

func (c *CreateSourceCommand) SQL() string {
	return c.sql
}

func (c *CreateSourceCommand) TableSequences() []uint64 {
	return c.tableSequences
}

func (c *CreateSourceCommand) LockName() string {
	return c.schemaName + "/"
}

func NewOriginatingCreateSourceCommand(e *Executor, schemaName string, sql string, tableSequences []uint64, ast *parser.CreateSource) *CreateSourceCommand {
	return &CreateSourceCommand{
		e:              e,
		schemaName:     schemaName,
		sql:            sql,
		tableSequences: tableSequences,
		ast:            ast,
	}
}

func NewCreateSourceCommand(e *Executor, schemaName string, sql string, tableSequences []uint64) *CreateSourceCommand {
	return &CreateSourceCommand{
		e:              e,
		schemaName:     schemaName,
		sql:            sql,
		tableSequences: tableSequences,
	}
}

func (c *CreateSourceCommand) BeforePrepare() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Before prepare we just persist the source info in the tables table
	var err error
	c.sourceInfo, err = c.getSourceInfo(c.ast)
	if err != nil {
		return err
	}
	_, ok := c.e.metaController.GetSource(c.schemaName, c.sourceInfo.Name)
	if ok {
		return perrors.NewSourceAlreadyExistsError(c.schemaName, c.sourceInfo.Name)
	}
	return c.e.metaController.PersistSource(c.sourceInfo, meta.PrepareStateAdd)
}

func (c *CreateSourceCommand) OnPrepare() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// If receiving on prepare from broadcast on the originating node, mvInfo will already be set
	// this means we do not have to parse the ast twice!
	if c.sourceInfo == nil {
		ast, err := parser.Parse(c.sql)
		if err != nil {
			return perrors.MaybeAddStack(err)
		}
		if ast.Create == nil || ast.Create.Source == nil {
			return fmt.Errorf("not a create source %s", c.sql)
		}
		c.sourceInfo, err = c.getSourceInfo(ast.Create.Source)
		if err != nil {
			return err
		}
	}
	// Create source in push engine so it can receive forwarded rows, do not activate consumers yet
	src, err := c.e.pushEngine.CreateSource(c.sourceInfo)
	if err != nil {
		return err
	}
	c.source = src
	return nil
}

func (c *CreateSourceCommand) OnCommit() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Activate the message consumers for the source
	if err := c.source.Start(); err != nil {
		return err
	}

	// Register the source in the in memory meta data
	return c.e.metaController.RegisterSource(c.sourceInfo)
}

func (c *CreateSourceCommand) AfterCommit() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Update row in metadata tables table to prepare=false
	return c.e.metaController.PersistSource(c.sourceInfo, meta.PrepareStateCommitted)
}

// nolint: gocyclo
func (c *CreateSourceCommand) getSourceInfo(ast *parser.CreateSource) (*common.SourceInfo, error) {
	var (
		colNames []string
		colTypes []common.ColumnType
		colIndex = map[string]int{}
		pkCols   []int
	)
	for i, option := range ast.Options {
		switch {
		case option.Column != nil:
			// Convert AST column definition to a ColumnType.
			col := option.Column
			colIndex[col.Name] = i
			colNames = append(colNames, col.Name)
			colType, err := col.ToColumnType()
			if err != nil {
				return nil, perrors.MaybeAddStack(err)
			}
			colTypes = append(colTypes, colType)

		case option.PrimaryKey != "":
			index, ok := colIndex[option.PrimaryKey]
			if !ok {
				return nil, fmt.Errorf("invalid primary key column %q", option.PrimaryKey)
			}
			pkCols = append(pkCols, index)

		default:
			panic(repr.String(option))
		}
	}

	var (
		headerEncoding, keyEncoding, valueEncoding common.KafkaEncoding
		propsMap                                   map[string]string
		colSelectors                               []string
		brokerName, topicName                      string
	)
	for _, opt := range ast.TopicInformation {
		switch {
		case opt.HeaderEncoding != "":
			headerEncoding = common.KafkaEncodingFromString(opt.HeaderEncoding)
			if headerEncoding.Encoding == common.EncodingUnknown {
				return nil, perrors.NewPranaErrorf(perrors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.HeaderEncoding)
			}
		case opt.KeyEncoding != "":
			keyEncoding = common.KafkaEncodingFromString(opt.KeyEncoding)
			if keyEncoding.Encoding == common.EncodingUnknown {
				return nil, perrors.NewPranaErrorf(perrors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.KeyEncoding)
			}
		case opt.ValueEncoding != "":
			valueEncoding = common.KafkaEncodingFromString(opt.ValueEncoding)
			if valueEncoding.Encoding == common.EncodingUnknown {
				return nil, perrors.NewPranaErrorf(perrors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.ValueEncoding)
			}
		case opt.Properties != nil:
			propsMap = make(map[string]string, len(opt.Properties))
			for _, prop := range opt.Properties {
				propsMap[prop.Key] = prop.Value
			}
		case opt.ColSelectors != nil:
			cs := opt.ColSelectors
			colSelectors = make([]string, len(cs))
			for i := 0; i < len(cs); i++ {
				colSelectors[i] = cs[i]
			}
		case opt.BrokerName != "":
			brokerName = opt.BrokerName
		case opt.TopicName != "":
			topicName = opt.TopicName
		}
	}
	if headerEncoding == common.KafkaEncodingUnknown {
		return nil, perrors.NewPranaError(perrors.InvalidStatement, "headerEncoding is required")
	}
	if keyEncoding == common.KafkaEncodingUnknown {
		return nil, perrors.NewPranaError(perrors.InvalidStatement, "keyEncoding is required")
	}
	if valueEncoding == common.KafkaEncodingUnknown {
		return nil, perrors.NewPranaError(perrors.InvalidStatement, "valueEncoding is required")
	}
	if brokerName == "" {
		return nil, perrors.NewPranaError(perrors.InvalidStatement, "brokerName is required")
	}
	if topicName == "" {
		return nil, perrors.NewPranaError(perrors.InvalidStatement, "topicName is required")
	}
	lc := len(colSelectors)
	if lc > 0 && lc != len(colTypes) {
		return nil, perrors.NewPranaErrorf(perrors.WrongNumberColumnSelectors,
			"Number of column selectors (%d) must match number of columns (%d)", lc, len(colTypes))
	}

	topicInfo := &common.TopicInfo{
		BrokerName:     brokerName,
		TopicName:      topicName,
		HeaderEncoding: headerEncoding,
		KeyEncoding:    keyEncoding,
		ValueEncoding:  valueEncoding,
		ColSelectors:   colSelectors,
		Properties:     propsMap,
	}
	tableInfo := common.TableInfo{
		ID:             c.tableSequences[0],
		SchemaName:     c.schemaName,
		Name:           ast.Name,
		PrimaryKeyCols: pkCols,
		ColumnNames:    colNames,
		ColumnTypes:    colTypes,
		IndexInfos:     nil,
	}
	return &common.SourceInfo{
		TableInfo: &tableInfo,
		TopicInfo: topicInfo,
	}, nil
}
