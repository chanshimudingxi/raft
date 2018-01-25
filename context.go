package raft

//上下文代表server的当前状态。在这里的上下文还有更强的语义，表明只能在同一个上下文里进行server状态的改变
// Context represents the current state of the server. It is passed into
// a command when the command is being applied since the server methods
// are locked.
type Context interface {
	Server() Server
	CurrentTerm() uint64  //当前任期号
	CurrentIndex() uint64 //当前索引号
	CommitIndex() uint64  //commit的索引号
}

// context is the concrete implementation of Context.
type context struct {
	server       Server
	currentIndex uint64
	currentTerm  uint64
	commitIndex  uint64
}

// Server returns a reference to the server.
func (c *context) Server() Server {
	return c.server
}

// CurrentTerm returns current term the server is in.
func (c *context) CurrentTerm() uint64 {
	return c.currentTerm
}

// CurrentIndex returns current index the server is at.
func (c *context) CurrentIndex() uint64 {
	return c.currentIndex
}

// CommitIndex returns last commit index the server is at.
func (c *context) CommitIndex() uint64 {
	return c.commitIndex
}
