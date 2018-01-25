package raft

//------------------------------------------------------------------------------
//
// Typedefs
//
//------------------------------------------------------------------------------

//Transporter接口是当前节点传输请求到其他节点的方法集
// Transporter is the interface for allowing the host application to transport
// requests to other nodes.
type Transporter interface {
	SendVoteRequest(server Server, peer *Peer, req *RequestVoteRequest) *RequestVoteResponse
	SendAppendEntriesRequest(server Server, peer *Peer, req *AppendEntriesRequest) *AppendEntriesResponse
	SendSnapshotRequest(server Server, peer *Peer, req *SnapshotRequest) *SnapshotResponse
	SendSnapshotRecoveryRequest(server Server, peer *Peer, req *SnapshotRecoveryRequest) *SnapshotRecoveryResponse
}
