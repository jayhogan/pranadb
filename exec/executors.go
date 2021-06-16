package exec

import (
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/storage"
)

type PushExecutor interface {
	HandleRows(rows *common.PushRows, ctx *ExecutionContext) error

	SetParent(parent PushExecutor)
	AddChild(parent PushExecutor)
	ReCalcSchema()
	ReCalcSchemaFromSources(colNames []string, colTypes []common.ColumnType, keyCols []int)

	ColNames() []string
	ColTypes() []common.ColumnType
	KeyCols() []int
}

type ExecutionContext struct {
	WriteBatch *storage.WriteBatch
	Forwarder  Forwarder
}

type Forwarder interface {
	QueueForRemoteSend(key []byte, row *common.PullRow, localShardID uint64, entityID uint64, colTypes []common.ColumnType, batch *storage.WriteBatch) error
}

type pushExecutorBase struct {
	colNames    []string
	colTypes    []common.ColumnType
	keyCols     []int
	rowsFactory *common.RowsFactory
	parent      PushExecutor
	children    []PushExecutor
}

func (p *pushExecutorBase) SetParent(parent PushExecutor) {
	p.parent = parent
}

func (p *pushExecutorBase) AddChild(child PushExecutor) {
	p.children = append(p.children, child)
}

func (p *pushExecutorBase) KeyCols() []int {
	return p.keyCols
}

func (p *pushExecutorBase) ColNames() []string {
	return p.colNames
}

func (p *pushExecutorBase) ColTypes() []common.ColumnType {
	return p.colTypes
}

func ConnectExecutors(childExecutors []PushExecutor, parent PushExecutor) {
	for _, child := range childExecutors {
		child.SetParent(parent)
		parent.AddChild(child)
	}
}
