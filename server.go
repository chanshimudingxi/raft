package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"sync"
	"time"
)

//------------------------------------------------------------------------------
//
// Constants
//
//------------------------------------------------------------------------------

//server的状态（raft有follower、candidate、leader三种状态）
const (
	Stopped      = "stopped"
	Initialized  = "initialized"
	Follower     = "follower"
	Candidate    = "candidate"
	Leader       = "leader"
	Snapshotting = "snapshotting"
)

const (
	MaxLogEntriesPerRequest         = 2000
	NumberOfLogEntriesAfterSnapshot = 200
)

const (
	//心跳间隔（leader发送给follower用来维持自己的任期）
	// DefaultHeartbeatInterval is the interval that the leader will send
	// AppendEntriesRequests to followers to maintain leadership.
	DefaultHeartbeatInterval = 50 * time.Millisecond

	//选举超时时间
	DefaultElectionTimeout = 150 * time.Millisecond
)

//按照raft协议的约束，心跳时间<<选举超时时间是预防选举进入死锁状态的重要手段
// ElectionTimeoutThresholdPercent specifies the threshold at which the server
// will dispatch warning events that the heartbeat RTT is too close to the
// election timeout.
const ElectionTimeoutThresholdPercent = 0.8

//------------------------------------------------------------------------------
//
// Errors
//
//------------------------------------------------------------------------------

var NotLeaderError = errors.New("raft.Server: Not current leader")
var DuplicatePeerError = errors.New("raft.Server: Duplicate peer")
var CommandTimeoutError = errors.New("raft: Command timeout")
var StopError = errors.New("raft: Has been stopped")

//------------------------------------------------------------------------------
//
// Typedefs
//
//------------------------------------------------------------------------------
//服务器（raft中角色的承载实体）的接口
// A server is involved in the consensus protocol and can act as a follower,
// candidate or a leader.
type Server interface {
	Name() string         //server名字
	Context() interface{} //
	StateMachine() StateMachine
	Leader() string                                        //leader名字
	State() string                                         //状态
	Path() string                                          //
	LogPath() string                                       //log的存储路径
	SnapshotPath(lastIndex uint64, lastTerm uint64) string //数据快照的存储路径
	Term() uint64                                          //任期号
	CommitIndex() uint64                                   //提交的index号
	VotedFor() string                                      //
	MemberCount() int                                      //成员个数
	QuorumSize() int
	IsLogEmpty() bool                                               //日志记录是否为空
	LogEntries() []*LogEntry                                        //获取日志记录
	LastCommandName() string                                        //上一个命令
	GetState() string                                               //获取server状态
	ElectionTimeout() time.Duration                                 //获取选举超时时间
	SetElectionTimeout(duration time.Duration)                      //设置选举超时时间
	HeartbeatInterval() time.Duration                               //获取心跳间隔时间
	SetHeartbeatInterval(duration time.Duration)                    //设置心跳间隔时间
	Transporter() Transporter                                       //
	SetTransporter(t Transporter)                                   //
	AppendEntries(req *AppendEntriesRequest) *AppendEntriesResponse //追加日志记录
	RequestVote(req *RequestVoteRequest) *RequestVoteResponse       //请求选票
	RequestSnapshot(req *SnapshotRequest) *SnapshotResponse         //请求的快照
	SnapshotRecoveryRequest(req *SnapshotRecoveryRequest) *SnapshotRecoveryResponse
	AddPeer(name string, connectiongString string) error //添加节点
	RemovePeer(name string) error                        //删除节点
	Peers() map[string]*Peer                             //获取节点
	Init() error                                         //server初始化
	Start() error                                        //server开始运行
	Stop()                                               //server停止运行
	Running() bool                                       //server是否处于运行状态
	Do(command Command) (interface{}, error)             //处理命令
	TakeSnapshot() error                                 //获取数据快照
	LoadSnapshot() error                                 //装载数据快照
	AddEventListener(string, EventListener)              //添加事件监听
	FlushCommitIndex()                                   //刷新提交的index
}

//服务
type server struct {
	*eventDispatcher //事件派发器

	name        string //名字
	path        string //路径
	state       string //状态
	transporter Transporter
	context     interface{} //
	currentTerm uint64      //当前的任期号

	votedFor   string           //投票的目标名字（候选人名字）
	log        *Log             //日志
	leader     string           //leader的名字
	peers      map[string]*Peer //集群节点
	mutex      sync.RWMutex     //读写锁
	syncedPeer map[string]bool  //

	stopped           chan bool     //
	c                 chan *ev      //事件channel
	electionTimeout   time.Duration //选举超时时间
	heartbeatInterval time.Duration //心跳间隔时间

	snapshot *Snapshot //数据快照（已落地到磁盘）

	// PendingSnapshot is an unfinished snapshot.
	// After the pendingSnapshot is saved to disk,
	// it will be set to snapshot and also will be
	// set to nil.
	pendingSnapshot *Snapshot //数据快照（尚未落地到磁盘的）

	stateMachine            StateMachine //
	maxLogEntriesPerRequest uint64       //

	connectionString string //

	routineGroup sync.WaitGroup //需要等待多个goroutine
}

//事件
// An internal event to be processed by the server's event loop.
type ev struct {
	target      interface{}
	returnValue interface{}
	c           chan error
}

//------------------------------------------------------------------------------
//
// Constructor
//
//------------------------------------------------------------------------------

//在指定log路径创建server。transporter不能为空。如果日志复制设置为不可用，状态机可以为空。
//上下文随意设置（不为raft包所用，而是由server返回）。
// Creates a new server with a log at the given path. transporter must
// not be nil. stateMachine can be nil if snapshotting and log
// compaction is to be disabled. context can be anything (including nil)
// and is not used by the raft package except returned by
// Server.Context(). connectionString can be anything.
func NewServer(name string, path string, transporter Transporter, stateMachine StateMachine, ctx interface{}, connectionString string) (Server, error) {
	if name == "" {
		return nil, errors.New("raft.Server: Name cannot be blank")
	}
	if transporter == nil {
		panic("raft: Transporter required")
	}

	s := &server{
		name:                    name,
		path:                    path,
		transporter:             transporter,
		stateMachine:            stateMachine,
		context:                 ctx,
		state:                   Stopped,
		peers:                   make(map[string]*Peer),
		log:                     newLog(),
		c:                       make(chan *ev, 256),
		electionTimeout:         DefaultElectionTimeout,
		heartbeatInterval:       DefaultHeartbeatInterval,
		maxLogEntriesPerRequest: MaxLogEntriesPerRequest,
		connectionString:        connectionString,
	}
	s.eventDispatcher = newEventDispatcher(s)

	// Setup apply function.
	s.log.ApplyFunc = func(e *LogEntry, c Command) (interface{}, error) {
		//分发commit事件
		// Dispatch commit event.
		s.DispatchEvent(newEvent(CommitEventType, e, nil))

		//将command应用到状态机
		// Apply command to the state machine.
		switch c := c.(type) { //c必须是接口类型，这个是golang里面的switch type语法
		case CommandApply:
			return c.Apply(&context{
				server:       s,
				currentTerm:  s.currentTerm,
				currentIndex: s.log.internalCurrentIndex(),
				commitIndex:  s.log.commitIndex,
			})
		case deprecatedCommandApply:
			return c.Apply(s)
		default:
			return nil, fmt.Errorf("Command does not implement Apply()")
		}
	}

	return s, nil
}

//------------------------------------------------------------------------------
//
// Accessors
//
//------------------------------------------------------------------------------

//--------------------------------------
// General
//--------------------------------------

//获取server的名字
// Retrieves the name of the server.
func (s *server) Name() string {
	return s.name
}

//获取server的存储路径
// Retrieves the storage path for the server.
func (s *server) Path() string {
	return s.path
}

//当前leader的名字
// The name of the current leader.
func (s *server) Leader() string {
	return s.leader
}

//获取节点数据的拷贝
// Retrieves a copy of the peer data.
func (s *server) Peers() map[string]*Peer {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	peers := make(map[string]*Peer)
	for name, peer := range s.peers {
		peers[name] = peer.clone()
	}
	return peers
}

//获取transporter
// Retrieves the object that transports requests.
func (s *server) Transporter() Transporter {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.transporter
}

//设置transporter
func (s *server) SetTransporter(t Transporter) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.transporter = t
}

//获取server的上下文
// Retrieves the context passed into the constructor.
func (s *server) Context() interface{} {
	return s.context
}

//获取server的状态机
// Retrieves the state machine passed into the constructor.
func (s *server) StateMachine() StateMachine {
	return s.stateMachine
}

//获取server的日志路径
// Retrieves the log path for the server.
func (s *server) LogPath() string {
	return path.Join(s.path, "log")
}

//获取server的当前状态
// Retrieves the current state of the server.
func (s *server) State() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.state
}

//设置server的状态
// Sets the state of the server.
func (s *server) setState(state string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Temporarily store previous values.
	prevState := s.state
	prevLeader := s.leader

	// Update state and leader.
	s.state = state
	if state == Leader {
		s.leader = s.Name()
		s.syncedPeer = make(map[string]bool)
	}

	//触发状态改变事件
	// Dispatch state and leader change events.
	s.DispatchEvent(newEvent(StateChangeEventType, s.state, prevState))

	//如果leader发生改变，触发leader改变事件
	if prevLeader != s.leader {
		s.DispatchEvent(newEvent(LeaderChangeEventType, s.leader, prevLeader))
	}
}

//获取server的任期号
// Retrieves the current term of the server.
func (s *server) Term() uint64 {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.currentTerm
}

//获取server的commit索引号
// Retrieves the current commit index of the server.
func (s *server) CommitIndex() uint64 {
	s.log.mutex.RLock()
	defer s.log.mutex.RUnlock()
	return s.log.commitIndex
}

//获取在这个任期内给其投票的候选人名字
// Retrieves the name of the candidate this server voted for in this term.
func (s *server) VotedFor() string {
	return s.votedFor
}

// Retrieves whether the server's log has no entries.
func (s *server) IsLogEmpty() bool {
	return s.log.isEmpty()
}

// A list of all the log entries. This should only be used for debugging purposes.
func (s *server) LogEntries() []*LogEntry {
	s.log.mutex.RLock()
	defer s.log.mutex.RUnlock()
	return s.log.entries
}

// A reference to the command name of the last entry.
func (s *server) LastCommandName() string {
	return s.log.lastCommandName()
}

//打印server的当前状态
// Get the state of the server for debugging
func (s *server) GetState() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return fmt.Sprintf("Name: %s, State: %s, Term: %v, CommitedIndex: %v ", s.name, s.state, s.currentTerm, s.log.commitIndex)
}

//检查server是否可以发起commit
// Check if the server is promotable
func (s *server) promotable() bool {
	return s.log.currentIndex() > 0
}

//--------------------------------------
// Membership 成员关系
//--------------------------------------

//获取一致同意的节点数
// Retrieves the number of member servers in the consensus.
func (s *server) MemberCount() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return len(s.peers) + 1
}

//获取投票通过需要的法定票数
// Retrieves the number of servers required to make a quorum.
func (s *server) QuorumSize() int {
	return (s.MemberCount() / 2) + 1
}

//--------------------------------------
// Election timeout 选举超时
//--------------------------------------

//获取选举超时时间
// Retrieves the election timeout.
func (s *server) ElectionTimeout() time.Duration {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.electionTimeout
}

//设置选举超时时间
// Sets the election timeout.
func (s *server) SetElectionTimeout(duration time.Duration) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.electionTimeout = duration
}

//--------------------------------------
// Heartbeat timeout 心跳时间
//--------------------------------------

// Retrieves the heartbeat timeout.
func (s *server) HeartbeatInterval() time.Duration {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.heartbeatInterval
}

// Sets the heartbeat timeout.
func (s *server) SetHeartbeatInterval(duration time.Duration) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.heartbeatInterval = duration
	for _, peer := range s.peers {
		peer.setHeartbeatInterval(duration)
	}
}

//------------------------------------------------------------------------------
//
// Methods
//
//------------------------------------------------------------------------------

//--------------------------------------
// Initialization
//--------------------------------------

// Reg the NOPCommand
func init() {
	RegisterCommand(&NOPCommand{})
	RegisterCommand(&DefaultJoinCommand{})
	RegisterCommand(&DefaultLeaveCommand{})
}

//启动raft server
//如果日志记录存在，且没有收到AppendEntries的话，允许变成候选人发起选举。
//如果没有日志记录存在，等待来自其他节点的AppendEntries
//如果没有日志记录存在，等待自连接命令出现，如果出现那么立刻成为leader并且提交日志记录。
// Start the raft server
// If log entries exist then allow promotion to candidate if no AEs received.
// If no log entries exist then wait for AEs from another node.
// If no log entries exist and a self-join command is issued then
// immediately become leader and commit entry.
func (s *server) Start() error {
	// Exit if the server is already running.
	if s.Running() {
		return fmt.Errorf("raft.Server: Server already running[%v]", s.state)
	}

	if err := s.Init(); err != nil {
		return err
	}

	// stopped needs to be allocated each time server starts
	// because it is closed at `Stop`.
	s.stopped = make(chan bool)
	s.setState(Follower)

	// If no log entries exist then
	// 1. wait for AEs from another node
	// 2. wait for self-join command
	// to set itself promotable
	if !s.promotable() {
		s.debugln("start as a new raft server")

		// If log entries exist then allow promotion to candidate
		// if no AEs received.
	} else {
		s.debugln("start from previous saved state")
	}

	debugln(s.GetState())

	s.routineGroup.Add(1)
	go func() {
		defer s.routineGroup.Done()
		s.loop()
	}()

	return nil
}

//初始化raft server
//如果在原来路径上没有日志文件，Init()会创建一个空文件。否则将从原来的日志文件加载日志记录
// Init initializes the raft server.
// If there is no previous log file under the given path, Init() will create an empty log file.
// Otherwise, Init() will load in the log entries from the log file.
func (s *server) Init() error {
	if s.Running() {
		return fmt.Errorf("raft.Server: Server already running[%v]", s.state)
	}

	// Server has been initialized or server was stopped after initialized
	// If log has been initialized, we know that the server was stopped after
	// running.
	if s.state == Initialized || s.log.initialized {
		s.state = Initialized
		return nil
	}

	//创建数据快照目录
	// Create snapshot directory if it does not exist
	err := os.Mkdir(path.Join(s.path, "snapshot"), 0700)
	if err != nil && !os.IsExist(err) {
		s.debugln("raft: Snapshot dir error: ", err)
		return fmt.Errorf("raft: Initialization error: %s", err)
	}

	if err := s.readConf(); err != nil {
		s.debugln("raft: Conf file error: ", err)
		return fmt.Errorf("raft: Initialization error: %s", err)
	}

	//加载日志记录
	// Initialize the log and load it up.
	if err := s.log.open(s.LogPath()); err != nil {
		s.debugln("raft: Log error: ", err)
		return fmt.Errorf("raft: Initialization error: %s", err)
	}

	//更新日志记录的最新任期号
	// Update the term to the last term in the log.
	_, s.currentTerm = s.log.lastInfo()

	s.state = Initialized
	return nil
}

//关闭raft server
// Shuts down the server.
func (s *server) Stop() {
	if s.State() == Stopped {
		return
	}

	close(s.stopped)

	// make sure all goroutines have stopped before we close the log
	s.routineGroup.Wait()

	s.log.close()
	s.setState(Stopped)
}

//检查server是否处于运行状态
// Checks if the server is currently running.
func (s *server) Running() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return (s.state != Stopped && s.state != Initialized)
}

//--------------------------------------
// Term 任期
//--------------------------------------

//更新任期号
// updates the current term for the server. This is only used when a larger
// external term is found.
func (s *server) updateCurrentTerm(term uint64, leaderName string) {
	_assert(term > s.currentTerm,
		"upadteCurrentTerm: update is called when term is not larger than currentTerm")

	// Store previous values temporarily.
	prevTerm := s.currentTerm
	prevLeader := s.leader

	//更新当前的任期号为T，并且变为follower状态
	// set currentTerm = T, convert to follower (§5.1)
	// stop heartbeats before step-down
	if s.state == Leader {
		for _, peer := range s.peers {
			peer.stopHeartbeat(false)
		}
	}
	// update the term and clear vote for
	if s.state != Follower {
		s.setState(Follower)
	}

	s.mutex.Lock()
	s.currentTerm = term
	s.leader = leaderName
	s.votedFor = ""
	s.mutex.Unlock()

	//分发任期变更事件
	// Dispatch change events.
	s.DispatchEvent(newEvent(TermChangeEventType, s.currentTerm, prevTerm))

	//分发leader变更事件
	if prevLeader != s.leader {
		s.DispatchEvent(newEvent(LeaderChangeEventType, s.leader, prevLeader))
	}
}

//--------------------------------------
// Event Loop 也就是状态机模型
//--------------------------------------

//               ________
//            --|Snapshot|                 timeout
//            |  --------                  ______
// recover    |       ^                   |      |
// snapshot / |       |snapshot           |      |
// higher     |       |                   v      |     recv majority votes
// term       |    --------    timeout    -----------                        -----------
//            |-> |Follower| ----------> | Candidate |--------------------> |  Leader   |
//                 --------               -----------                        -----------
//                    ^          higher term/ |                         higher term |
//                    |            new leader |                                     |
//                    |_______________________|____________________________________ |
// The main event loop for the server
func (s *server) loop() {
	defer s.debugln("server.loop.end")

	state := s.State()

	for state != Stopped {
		s.debugln("server.loop.run ", state)
		switch state {
		case Follower:
			s.followerLoop()
		case Candidate:
			s.candidateLoop()
		case Leader:
			s.leaderLoop()
		case Snapshotting:
			s.snapshotLoop()
		}
		state = s.State()
	}
}

//同步接口：发送事件到事件循环去进行处理。这是个阻塞函数，阻塞到事件被处理完毕为止。
// Sends an event to the event loop to be processed. The function will wait
// until the event is actually processed before returning.
func (s *server) send(value interface{}) (interface{}, error) {
	if !s.Running() {
		return nil, StopError
	}

	event := &ev{target: value, c: make(chan error, 1)}
	select {
	case s.c <- event:
	case <-s.stopped:
		return nil, StopError
	}
	select {
	case <-s.stopped:
		return nil, StopError
	case err := <-event.c:
		return event.returnValue, err
	}
}

//异步接口
func (s *server) sendAsync(value interface{}) {
	if !s.Running() {
		return
	}

	event := &ev{target: value, c: make(chan error, 1)}
	// try a non-blocking send first
	// in most cases, this should not be blocking
	// avoid create unnecessary go routines
	select {
	case s.c <- event:
		return
	default:
	}

	s.routineGroup.Add(1)
	go func() {
		defer s.routineGroup.Done()
		select {
		case s.c <- event:
		case <-s.stopped:
		}
	}()
}

//follower状态下的事件循环逻辑。
//一.处理候选人和领头羊的RPC请求
//二.不包括以下几种情况，如果选举超时，那么变为候选人：
//	1.收到合法的AppendEntries RPC
//	2.正在给候选人投票
// The event loop that is run when the server is in a Follower state.
// Responds to RPCs from candidates and leaders.
// Converts to candidate if election timeout elapses without either:
//   1.Receiving valid AppendEntries RPC, or
//   2.Granting vote to candidate
func (s *server) followerLoop() {
	since := time.Now()
	electionTimeout := s.ElectionTimeout()
	timeoutChan := afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)

	//follower的事件循环
	for s.State() == Follower {
		var err error
		update := false
		select {
		case <-s.stopped:
			s.setState(Stopped)
			return

		case e := <-s.c:
			switch req := e.target.(type) {
			case JoinCommand: //加入集群请求
				//If no log entries exist and a self-join command is issued
				//then immediately become leader and commit entry.
				if s.log.currentIndex() == 0 && req.NodeName() == s.Name() {
					s.debugln("selfjoin and promote to leader")
					s.setState(Leader)
					s.processCommand(req, e)
				} else {
					err = NotLeaderError
				}
			case *AppendEntriesRequest: //追加日志记录命令
				// If heartbeats get too close to the election timeout then send an event.
				elapsedTime := time.Now().Sub(since)
				if elapsedTime > time.Duration(float64(electionTimeout)*ElectionTimeoutThresholdPercent) {
					s.DispatchEvent(newEvent(ElectionTimeoutThresholdEventType, elapsedTime, nil))
				}
				e.returnValue, update = s.processAppendEntriesRequest(req)
			case *RequestVoteRequest: //请求投票请求
				e.returnValue, update = s.processRequestVoteRequest(req)
			case *SnapshotRequest: //快照请求
				e.returnValue = s.processSnapshotRequest(req)
			default:
				err = NotLeaderError
			}
			// Callback to event.
			e.c <- err

		case <-timeoutChan:
			// only allow synced follower to promote to candidate
			if s.promotable() {
				s.setState(Candidate)
			} else {
				update = true
			}
		}

		// Converts to candidate if election timeout elapses without either:
		//   1.Receiving valid AppendEntries RPC, or
		//   2.Granting vote to candidate
		if update {
			since = time.Now()
			timeoutChan = afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)
		}
	}
}

//candicate状态下的事件循环
// The event loop that is run when the server is in a Candidate state.
func (s *server) candidateLoop() {
	// Clear leader value.
	prevLeader := s.leader
	s.leader = ""
	if prevLeader != s.leader {
		s.DispatchEvent(newEvent(LeaderChangeEventType, s.leader, prevLeader))
	}

	lastLogIndex, lastLogTerm := s.log.lastInfo()
	doVote := true
	votesGranted := 0
	var timeoutChan <-chan time.Time
	var respChan chan *RequestVoteResponse

	for s.State() == Candidate {
		if doVote {
			//任期号加一
			// Increment current term, vote for self.
			s.currentTerm++
			s.votedFor = s.name

			//发送投票请求到所有server
			// Send RequestVote RPCs to all other servers.
			respChan = make(chan *RequestVoteResponse, len(s.peers))
			for _, peer := range s.peers {
				s.routineGroup.Add(1)
				go func(peer *Peer) {
					defer s.routineGroup.Done()
					peer.sendVoteRequest(newRequestVoteRequest(s.currentTerm, s.name, lastLogIndex, lastLogTerm), respChan)
				}(peer)
			}

			//等待条件
			//1-收到大多数server的投票，成为leader
			//2-收到新leader的追加日志记录RPC，停止投票，变为follower
			// Wait for either:
			//   * Votes received from majority of servers: become leader
			//   * AppendEntries RPC received from new leader: step down.
			//   * Election timeout elapses without election resolution: increment term, start new election
			//   * Discover higher term: step down (§5.1)
			votesGranted = 1
			timeoutChan = afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)
			doVote = false
		}

		//收到法定票数
		// If we received enough votes then stop waiting for more votes.
		// And return from the candidate loop
		if votesGranted == s.QuorumSize() {
			s.debugln("server.candidate.recv.enough.votes")
			s.setState(Leader)
			return
		}
		//从各个节点收集选票
		// Collect votes from peers.
		select {
		case <-s.stopped:
			s.setState(Stopped)
			return

		case resp := <-respChan:
			if success := s.processVoteResponse(resp); success {
				s.debugln("server.candidate.vote.granted: ", votesGranted)
				votesGranted++
			}

		case e := <-s.c:
			var err error
			switch req := e.target.(type) {
			case Command:
				err = NotLeaderError
			case *AppendEntriesRequest:
				e.returnValue, _ = s.processAppendEntriesRequest(req)
			case *RequestVoteRequest:
				e.returnValue, _ = s.processRequestVoteRequest(req)
			}

			// Callback to event.
			e.c <- err

		case <-timeoutChan:
			doVote = true
		}
	}
}

//leader状态的事件循环
// The event loop that is run when the server is in a Leader state.
func (s *server) leaderLoop() {
	logIndex, _ := s.log.lastInfo()

	// Update the peers prevLogIndex to leader's lastLogIndex and start heartbeat.
	s.debugln("leaderLoop.set.PrevIndex to ", logIndex)
	for _, peer := range s.peers {
		peer.setPrevLogIndex(logIndex)
		peer.startHeartbeat()
	}

	//发送心跳
	// Commit a NOP after the server becomes leader. From the Raft paper:
	// "Upon election: send initial empty AppendEntries RPCs (heartbeat) to
	// each server; repeat during idle periods to prevent election timeouts
	// (§5.2)". The heartbeats started above do the "idle" period work.
	s.routineGroup.Add(1)
	go func() {
		defer s.routineGroup.Done()
		s.Do(NOPCommand{})
	}()

	//收集来自follower的请求
	// Begin to collect response from followers
	for s.State() == Leader {
		var err error
		select {
		case <-s.stopped:
			// Stop all peers before stop
			for _, peer := range s.peers {
				peer.stopHeartbeat(false)
			}
			s.setState(Stopped)
			return

		case e := <-s.c:
			switch req := e.target.(type) {
			case Command:
				s.processCommand(req, e)
				continue
			case *AppendEntriesRequest:
				e.returnValue, _ = s.processAppendEntriesRequest(req)
			case *AppendEntriesResponse:
				s.processAppendEntriesResponse(req)
			case *RequestVoteRequest:
				e.returnValue, _ = s.processRequestVoteRequest(req)
			}

			// Callback to event.
			e.c <- err
		}
	}

	s.syncedPeer = nil
}

func (s *server) snapshotLoop() {
	for s.State() == Snapshotting {
		var err error
		select {
		case <-s.stopped:
			s.setState(Stopped)
			return

		case e := <-s.c:
			switch req := e.target.(type) {
			case Command:
				err = NotLeaderError
			case *AppendEntriesRequest:
				e.returnValue, _ = s.processAppendEntriesRequest(req)
			case *RequestVoteRequest:
				e.returnValue, _ = s.processRequestVoteRequest(req)
			case *SnapshotRecoveryRequest:
				e.returnValue = s.processSnapshotRecoveryRequest(req)
			}
			// Callback to event.
			e.c <- err
		}
	}
}

//--------------------------------------
// Commands 命令
//--------------------------------------

// Attempts to execute a command and replicate it. The function will return
// when the command has been successfully committed or an error has occurred.

func (s *server) Do(command Command) (interface{}, error) {
	return s.send(command)
}

//处理命令
// Processes a command.
func (s *server) processCommand(command Command, e *ev) {
	s.debugln("server.command.process")

	// Create an entry for the command in the log.
	entry, err := s.log.createEntry(s.currentTerm, command, e)

	if err != nil {
		s.debugln("server.command.log.entry.error:", err)
		e.c <- err
		return
	}

	if err := s.log.appendEntry(entry); err != nil {
		s.debugln("server.command.log.error:", err)
		e.c <- err
		return
	}

	s.syncedPeer[s.Name()] = true
	if len(s.peers) == 0 {
		commitIndex := s.log.currentIndex()
		s.log.setCommitIndex(commitIndex)
		s.debugln("commit index ", commitIndex)
	}
}

//--------------------------------------
// Append Entries 追加日志记录
//--------------------------------------

// Appends zero or more log entry from the leader to this server.
func (s *server) AppendEntries(req *AppendEntriesRequest) *AppendEntriesResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*AppendEntriesResponse)
	return resp
}

// Processes the "append entries" request.
func (s *server) processAppendEntriesRequest(req *AppendEntriesRequest) (*AppendEntriesResponse, bool) {
	s.traceln("server.ae.process")

	//请求携带的任期号如果小于当前server的任期号，那么这个命令是不能被处理的
	if req.Term < s.currentTerm {
		s.debugln("server.ae.error: stale term")
		return newAppendEntriesResponse(s.currentTerm, false, s.log.currentIndex(), s.log.CommitIndex()), false
	}

	if req.Term == s.currentTerm { //任期号相同
		_assert(s.State() != Leader, "leader.elected.at.same.term.%d\n", s.currentTerm)

		// step-down to follower when it is a candidate
		if s.state == Candidate {
			// change state to follower
			s.setState(Follower)
		}

		// discover new leader when candidate
		// save leader name when follower
		s.leader = req.LeaderName
	} else { //任期号不同
		// Update term and leader.
		s.updateCurrentTerm(req.Term, req.LeaderName)
	}

	//拒绝如果log出现日志记录不匹配
	// Reject if log doesn't contain a matching previous entry.
	if err := s.log.truncate(req.PrevLogIndex, req.PrevLogTerm); err != nil {
		s.debugln("server.ae.truncate.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.currentIndex(), s.log.CommitIndex()), true
	}

	//log追加日志记录
	// Append entries to the log.
	if err := s.log.appendEntries(req.Entries); err != nil {
		s.debugln("server.ae.append.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.currentIndex(), s.log.CommitIndex()), true
	}

	//接受commit索引号
	// Commit up to the commit index.
	if err := s.log.setCommitIndex(req.CommitIndex); err != nil {
		s.debugln("server.ae.commit.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.currentIndex(), s.log.CommitIndex()), true
	}

	// once the server appended and committed all the log entries from the leader

	return newAppendEntriesResponse(s.currentTerm, true, s.log.currentIndex(), s.log.CommitIndex()), true
}

// Processes the "append entries" response from the peer. This is only
// processed when the server is a leader. Responses received during other
// states are dropped.
func (s *server) processAppendEntriesResponse(resp *AppendEntriesResponse) {
	// If we find a higher term then change to a follower and exit.
	if resp.Term() > s.Term() {
		s.updateCurrentTerm(resp.Term(), "")
		return
	}

	// panic response if it's not successful.
	if !resp.Success() {
		return
	}

	// if one peer successfully append a log from the leader term,
	// we add it to the synced list
	if resp.append == true {
		s.syncedPeer[resp.peer] = true
	}

	// Increment the commit count to make sure we have a quorum before committing.
	if len(s.syncedPeer) < s.QuorumSize() {
		return
	}

	//确定大多数节点有这个commit索引号
	// Determine the committed index that a majority has.
	var indices []uint64
	indices = append(indices, s.log.currentIndex())
	for _, peer := range s.peers {
		indices = append(indices, peer.getPrevLogIndex())
	}
	sort.Sort(sort.Reverse(uint64Slice(indices)))

	//接受commit索引号当大多数节点都已经追加了
	// We can commit up to the index which the majority of the members have appended.
	commitIndex := indices[s.QuorumSize()-1]
	committedIndex := s.log.commitIndex

	if commitIndex > committedIndex {
		//leader接受commit索引号之前必须先落地
		// leader needs to do a fsync before committing log entries
		s.log.sync()
		s.log.setCommitIndex(commitIndex)
		s.debugln("commit index ", commitIndex)
	}
}

//处理投票请求：
//1.如果投票同意候选人当前任期，返回true
//2.如果由于任期号小于对方被拒绝的，更新自己的任期号，返回false。
//3.如果投票任期号小于自己的任期号，忽略请求，返回false。
// processVoteReponse processes a vote request:
// 1. if the vote is granted for the current term of the candidate, return true
// 2. if the vote is denied due to smaller term, update the term of this server
//    which will also cause the candidate to step-down, and return false.
// 3. if the vote is for a smaller term, ignore it and return false.
func (s *server) processVoteResponse(resp *RequestVoteResponse) bool {
	if resp.VoteGranted && resp.Term == s.currentTerm {
		return true
	}

	if resp.Term > s.currentTerm {
		s.debugln("server.candidate.vote.failed")
		s.updateCurrentTerm(resp.Term, "")
	} else {
		s.debugln("server.candidate.vote: denied")
	}
	return false
}

//--------------------------------------
// Request Vote
//--------------------------------------
//投票的条件：
//1.自己还没有投票且任期相等。
//2.自己的任期号小于对方任期号。
// Requests a vote from a server. A vote can be obtained if the vote's term is
// at the server's current term and the server has not made a vote yet. A vote
// can also be obtained if the term is greater than the server's current term.
func (s *server) RequestVote(req *RequestVoteRequest) *RequestVoteResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*RequestVoteResponse)
	return resp
}

//处理“投票请求”
// Processes a "request vote" request.
func (s *server) processRequestVoteRequest(req *RequestVoteRequest) (*RequestVoteResponse, bool) {

	//如果投票任期号小于自己的任期号，忽略请求。
	// If the request is coming from an old term then reject it.
	if req.Term < s.Term() {
		s.debugln("server.rv.deny.vote: cause stale term")
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	//如果由于对方任期号大于自己的，更新自己的任期号；如果任期号相等且已经投票，那就不能在投票。
	// If the term of the request peer is larger than this node, update the term
	// If the term is equal and we've already voted for a different candidate then
	// don't vote for this candidate.
	if req.Term > s.Term() {
		s.updateCurrentTerm(req.Term, "")
	} else if s.votedFor != "" && s.votedFor != req.CandidateName {
		s.debugln("server.deny.vote: cause duplicate vote: ", req.CandidateName,
			" already vote for ", s.votedFor)
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	//如果候选人的最后日志记录没有自己的新，不能投票
	// If the candidate's log is not at least as up-to-date as our last log then don't vote.
	lastIndex, lastTerm := s.log.lastInfo()
	if lastIndex > req.LastLogIndex || lastTerm > req.LastLogTerm {
		s.debugln("server.deny.vote: cause out of date log: ", req.CandidateName,
			"Index :[", lastIndex, "]", " [", req.LastLogIndex, "]",
			"Term :[", lastTerm, "]", " [", req.LastLogTerm, "]")
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	//到止为止都满足条件，那就投票给候选人，并且重置自己的选举超时时间。
	// If we made it this far then cast a vote and reset our election time out.
	s.debugln("server.rv.vote: ", s.name, " votes for", req.CandidateName, "at term", req.Term)
	s.votedFor = req.CandidateName

	return newRequestVoteResponse(s.currentTerm, true), true
}

//--------------------------------------
// Membership 成员关系
//--------------------------------------

//添加新节点
// Adds a peer to the server.
func (s *server) AddPeer(name string, connectiongString string) error {
	s.debugln("server.peer.add: ", name, len(s.peers))

	// Do not allow peers to be added twice.
	if s.peers[name] != nil {
		return nil
	}

	// Skip the Peer if it has the same name as the Server
	if s.name != name {
		peer := newPeer(s, name, connectiongString, s.heartbeatInterval)

		if s.State() == Leader {
			peer.startHeartbeat()
		}

		s.peers[peer.Name] = peer

		s.DispatchEvent(newEvent(AddPeerEventType, name, nil))
	}

	// Write the configuration to file.
	s.writeConf()

	return nil
}

//删除节点
// Removes a peer from the server.
func (s *server) RemovePeer(name string) error {
	s.debugln("server.peer.remove: ", name, len(s.peers))

	// Skip the Peer if it has the same name as the Server
	if name != s.Name() {
		// Return error if peer doesn't exist.
		peer := s.peers[name]
		if peer == nil {
			return fmt.Errorf("raft: Peer not found: %s", name)
		}

		// Stop peer and remove it.
		if s.State() == Leader {
			// We create a go routine here to avoid potential deadlock.
			// We are holding log write lock when reach this line of code.
			// Peer.stopHeartbeat can be blocked without go routine, if the
			// target go routine (which we want to stop) is calling
			// log.getEntriesAfter and waiting for log read lock.
			// So we might be holding log lock and waiting for log lock,
			// which lead to a deadlock.
			// TODO(xiangli) refactor log lock
			s.routineGroup.Add(1)
			go func() {
				defer s.routineGroup.Done()
				peer.stopHeartbeat(true)
			}()
		}

		delete(s.peers, name)

		s.DispatchEvent(newEvent(RemovePeerEventType, name, nil))
	}

	// Write the configuration to file.
	s.writeConf()

	return nil
}

//--------------------------------------
// Log compaction 日志压缩
//--------------------------------------

func (s *server) TakeSnapshot() error {
	if s.stateMachine == nil {
		return errors.New("Snapshot: Cannot create snapshot. Missing state machine.")
	}

	// Shortcut without lock
	// Exit if the server is currently creating a snapshot.
	if s.pendingSnapshot != nil {
		return errors.New("Snapshot: Last snapshot is not finished.")
	}

	// TODO: acquire the lock and no more committed is allowed
	// This will be done after finishing refactoring heartbeat
	s.debugln("take.snapshot")

	lastIndex, lastTerm := s.log.commitInfo()

	// check if there is log has been committed since the
	// last snapshot.
	if lastIndex == s.log.startIndex {
		return nil
	}

	path := s.SnapshotPath(lastIndex, lastTerm)
	// Attach snapshot to pending snapshot and save it to disk.
	s.pendingSnapshot = &Snapshot{lastIndex, lastTerm, nil, nil, path}

	state, err := s.stateMachine.Save()
	if err != nil {
		return err
	}

	// Clone the list of peers.
	peers := make([]*Peer, 0, len(s.peers)+1)
	for _, peer := range s.peers {
		peers = append(peers, peer.clone())
	}
	peers = append(peers, &Peer{Name: s.Name(), ConnectionString: s.connectionString})

	// Attach snapshot to pending snapshot and save it to disk.
	s.pendingSnapshot.Peers = peers
	s.pendingSnapshot.State = state
	s.saveSnapshot()

	// We keep some log entries after the snapshot.
	// We do not want to send the whole snapshot to the slightly slow machines
	if lastIndex-s.log.startIndex > NumberOfLogEntriesAfterSnapshot {
		compactIndex := lastIndex - NumberOfLogEntriesAfterSnapshot
		compactTerm := s.log.getEntry(compactIndex).Term()
		s.log.compact(compactIndex, compactTerm)
	}

	return nil
}

//获取server的log路径
// Retrieves the log path for the server.
func (s *server) saveSnapshot() error {
	if s.pendingSnapshot == nil {
		return errors.New("pendingSnapshot.is.nil")
	}

	// Write snapshot to disk.
	if err := s.pendingSnapshot.save(); err != nil {
		return err
	}

	// Swap the current and last snapshots.
	tmp := s.snapshot
	s.snapshot = s.pendingSnapshot

	// Delete the previous snapshot if there is any change
	if tmp != nil && !(tmp.LastIndex == s.snapshot.LastIndex && tmp.LastTerm == s.snapshot.LastTerm) {
		tmp.remove()
	}
	s.pendingSnapshot = nil

	return nil
}

//获取server的log路径
// Retrieves the log path for the server.
func (s *server) SnapshotPath(lastIndex uint64, lastTerm uint64) string {
	return path.Join(s.path, "snapshot", fmt.Sprintf("%v_%v.ss", lastTerm, lastIndex))
}

func (s *server) RequestSnapshot(req *SnapshotRequest) *SnapshotResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*SnapshotResponse)
	return resp
}

//处理数据快照请求
func (s *server) processSnapshotRequest(req *SnapshotRequest) *SnapshotResponse {
	// If the follower’s log contains an entry at the snapshot’s last index with a term
	// that matches the snapshot’s last term, then the follower already has all the
	// information found in the snapshot and can reply false.
	entry := s.log.getEntry(req.LastIndex)

	if entry != nil && entry.Term() == req.LastTerm {
		return newSnapshotResponse(false)
	}

	// Update state.
	s.setState(Snapshotting)

	return newSnapshotResponse(true)
}

func (s *server) SnapshotRecoveryRequest(req *SnapshotRecoveryRequest) *SnapshotRecoveryResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*SnapshotRecoveryResponse)
	return resp
}

//处理数据快照恢复请求
func (s *server) processSnapshotRecoveryRequest(req *SnapshotRecoveryRequest) *SnapshotRecoveryResponse {
	// Recover state sent from request.
	if err := s.stateMachine.Recovery(req.State); err != nil {
		panic("cannot recover from previous state")
	}

	// Recover the cluster configuration.
	s.peers = make(map[string]*Peer)
	for _, peer := range req.Peers {
		s.AddPeer(peer.Name, peer.ConnectionString)
	}

	// Update log state.
	s.currentTerm = req.LastTerm
	s.log.updateCommitIndex(req.LastIndex)

	// Create local snapshot.
	s.pendingSnapshot = &Snapshot{req.LastIndex, req.LastTerm, req.Peers, req.State, s.SnapshotPath(req.LastIndex, req.LastTerm)}
	s.saveSnapshot()

	// Clear the previous log entries.
	s.log.compact(req.LastIndex, req.LastTerm)

	return newSnapshotRecoveryResponse(req.LastTerm, true, req.LastIndex)
}

//重启服务的时候加载数据快照
// Load a snapshot at restart
func (s *server) LoadSnapshot() error {
	// Open snapshot/ directory.
	dir, err := os.OpenFile(path.Join(s.path, "snapshot"), os.O_RDONLY, 0)
	if err != nil {
		s.debugln("cannot.open.snapshot: ", err)
		return err
	}

	// Retrieve a list of all snapshots.
	filenames, err := dir.Readdirnames(-1)
	if err != nil {
		dir.Close()
		panic(err)
	}
	dir.Close()

	if len(filenames) == 0 {
		s.debugln("no.snapshot.to.load")
		return nil
	}

	// Grab the latest snapshot.
	sort.Strings(filenames)
	snapshotPath := path.Join(s.path, "snapshot", filenames[len(filenames)-1])

	// Read snapshot data.
	file, err := os.OpenFile(snapshotPath, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	// Check checksum.
	var checksum uint32
	n, err := fmt.Fscanf(file, "%08x\n", &checksum)
	if err != nil {
		return err
	} else if n != 1 {
		return errors.New("checksum.err: bad.snapshot.file")
	}

	// Load remaining snapshot contents.
	b, err := ioutil.ReadAll(file)
	if err != nil {
		return err
	}

	// Generate checksum.
	byteChecksum := crc32.ChecksumIEEE(b)
	if uint32(checksum) != byteChecksum {
		s.debugln(checksum, " ", byteChecksum)
		return errors.New("bad snapshot file")
	}

	// Decode snapshot.
	if err = json.Unmarshal(b, &s.snapshot); err != nil {
		s.debugln("unmarshal.snapshot.error: ", err)
		return err
	}

	// Recover snapshot into state machine.
	if err = s.stateMachine.Recovery(s.snapshot.State); err != nil {
		s.debugln("recovery.snapshot.error: ", err)
		return err
	}

	// Recover cluster configuration.
	for _, peer := range s.snapshot.Peers {
		s.AddPeer(peer.Name, peer.ConnectionString)
	}

	// Update log state.
	s.log.startTerm = s.snapshot.LastTerm
	s.log.startIndex = s.snapshot.LastIndex
	s.log.updateCommitIndex(s.snapshot.LastIndex)

	return err
}

//--------------------------------------
// Config File 配置文件
//--------------------------------------

//刷新commit索引号到磁盘。
// Flushes commit index to the disk.
// So when the raft server restarts, it will commit upto the flushed commitIndex.
func (s *server) FlushCommitIndex() {
	s.debugln("server.conf.update")
	// Write the configuration to file.
	s.writeConf()
}

func (s *server) writeConf() {

	peers := make([]*Peer, len(s.peers))

	i := 0
	for _, peer := range s.peers {
		peers[i] = peer.clone()
		i++
	}

	r := &Config{
		CommitIndex: s.log.commitIndex,
		Peers:       peers,
	}

	b, _ := json.Marshal(r)

	confPath := path.Join(s.path, "conf")
	tmpConfPath := path.Join(s.path, "conf.tmp")

	err := writeFileSynced(tmpConfPath, b, 0600)

	if err != nil {
		panic(err)
	}

	os.Rename(tmpConfPath, confPath)
}

//读取server的配置
// Read the configuration for the server.
func (s *server) readConf() error {
	confPath := path.Join(s.path, "conf")
	s.debugln("readConf.open ", confPath)

	// open conf file
	b, err := ioutil.ReadFile(confPath)

	if err != nil {
		return nil
	}

	conf := &Config{}

	if err = json.Unmarshal(b, conf); err != nil {
		return err
	}

	s.log.updateCommitIndex(conf.CommitIndex)

	return nil
}

//--------------------------------------
// Debugging 调试
//--------------------------------------

func (s *server) debugln(v ...interface{}) {
	if logLevel > Debug {
		debugf("[%s Term:%d] %s", s.name, s.Term(), fmt.Sprintln(v...))
	}
}

func (s *server) traceln(v ...interface{}) {
	if logLevel > Trace {
		tracef("[%s] %s", s.name, fmt.Sprintln(v...))
	}
}
