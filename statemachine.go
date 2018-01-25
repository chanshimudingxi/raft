package raft

//状态机提供raft的两个功能：数据快照和日志压缩，目的是保存server的整个状态机状态，让server从状态机状态中恢复
// StateMachine is the interface for allowing the host application to save and
// recovery the state machine. This makes it possible to make snapshots
// and compact the log.
type StateMachine interface {
	Save() ([]byte, error)
	Recovery([]byte) error
}
