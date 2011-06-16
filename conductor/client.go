/* client.go
 *
 * Client Handling
*/

package main
import (
	o "orchestra"
	"net"
	"time"
	"os"
)

const (
//	KeepaliveDelay =	300e9 // once every 5 minutes.
	KeepaliveDelay =	10e9 // once every 10 seconds for debug
	RetryDelay     =  	5e9 // retry every 5 seconds.  Must be smaller than the keepalive to avoid channel race.
	OutputQueueDepth =	10
)


type ClientInfo struct {
	Player		string
	PktOutQ		chan *o.WirePkt
	PktInQ		chan *o.WirePkt
	abortQ		chan int
	TaskQ		chan *o.TaskRequest
	connection	net.Conn
	pendingTasks	map[uint64]*o.TaskRequest
}

func NewClientInfo() (client *ClientInfo) {
	client = new(ClientInfo)
	client.abortQ = make(chan int, 2)
	client.PktOutQ = make(chan *o.WirePkt, OutputQueueDepth)
	client.PktInQ = make(chan *o.WirePkt)
	client.TaskQ = make(chan *o.TaskRequest)

	return client
}

func (client *ClientInfo) Abort() {
	PlayerDied(client)
	reg := ClientGet(client.Player)
	if reg != nil {
		reg.Disassociate()
	}
	client.abortQ <- 1;
}

func (client *ClientInfo) Name() (name string) {
	if client.Player == "" {
		return "UNK:" + client.connection.RemoteAddr().String()
	}
	return client.Player
}

func (client *ClientInfo) SendTask(task *o.TaskRequest) {
	tr := task.Encode()
	p, err := o.Encode(tr)
	o.MightFail("Couldn't encode task for client.", err)
	client.PktOutQ <- p;
	task.RetryTime = time.Nanoseconds() + RetryDelay
}

func (client *ClientInfo) GotTask(task *o.TaskRequest) {
	/* first up, look at the task state */
	switch (task.State) {
	case o.TASK_QUEUED:
		fallthrough
	case o.TASK_PENDINGRESULT:
		/* this is a new task.  We should send it straight */
		task.Player = client.Player
		task.State = o.TASK_PENDINGRESULT

		client.pendingTasks[task.Job.Id] = task
		client.SendTask(task)
	case o.TASK_FINISHED:
		/* discard.  We don't care about tasks that are done. */		
	}
}

// this merges the state from the registry record into the client it's called against.
// it also copies back the active communication channels to the registry record.
func (client *ClientInfo) MergeState(regrecord *ClientInfo) {
	client.Player = regrecord.Player
	client.pendingTasks = regrecord.pendingTasks

	regrecord.TaskQ = client.TaskQ
	regrecord.abortQ = client.abortQ
	regrecord.PktOutQ = client.PktOutQ
	regrecord.PktInQ = client.PktInQ
	regrecord.connection = client.connection
}

// Sever the connection state from the client (used against registry records only)
func (client *ClientInfo) Disassociate() {
	client.TaskQ = nil
	client.abortQ = nil
	client.PktInQ = nil
	client.PktOutQ = nil
	client.connection = nil
}

func handleNop(client *ClientInfo, message interface{}) {
	o.Warn("Client %s: NOP Received", client.Name())
}

func handleIdentify(client *ClientInfo, message interface{}) {
	if client.Player != "" {
		o.Warn("Client %s: Tried to reintroduce itself.", client.Name())
		client.Abort()
		return
	}
	ic, _ := message.(*o.IdentifyClient)
	o.Warn("Client %s: Identified Itself As \"%s\"", client.Name(), *ic.Hostname)
	client.Player = *ic.Hostname
	if (!HostAuthorised(client.Player)) {
		o.Warn("Client %s: Not Authorised", client.Name())
		client.Abort()
		return
	}
	reg := ClientGet(client.Player)
	if nil == reg {
		o.Warn("Couldn't register client %s.  aborting connection.", client.Name())
		client.Abort()
	}
	client.MergeState(reg)
}

func handleReadyForTask(client *ClientInfo, message interface{}) {
	o.Warn("Client %s: Asked for Job", client.Name())
	PlayerWaitingForJob(client)
}

func handleIllegal(client *ClientInfo, message interface{}) {
	o.Warn("Client %s: Sent Illegal Message")
	client.Abort()
}

func handleResult(client *ClientInfo, message interface{}){
	jr, _ := message.(*o.ProtoTaskResponse)
	r := o.ResponseFromProto(jr)
	// at this point in time, we only care about terminal
	// condition codes.  a Job that isn't finished is just
	// prodding us back to let us know it lives.
	if r.IsFinished() {
		job := o.JobGet(r.Id)
		if nil == job {
			nack := o.MakeNack(r.Id)
			client.PktOutQ <- nack
		} else {
			job := o.JobGet(r.Id)
			if job != nil {
				/* if the job exists, Ack it. */
				ack := o.MakeAck(r.Id)
				client.PktOutQ <- ack
			}
			// now, we only accept the results if we were
			// expecting the results (ie: it was pending)
			// and expunge the task information from the
			// pending list so we stop bugging the client for it.
			task, exists := client.pendingTasks[r.Id]
			if exists {
				o.JobAddResult(client.Player, r)
				//FIXME: we also need to review the job's state now
				task.State = o.TASK_FINISHED
				client.pendingTasks[r.Id] = nil, false
			}
		}
	}
}


var dispatcher	= map[uint8] func(*ClientInfo,interface{}) {
	o.TypeNop:		handleNop,
	o.TypeIdentifyClient:	handleIdentify,
	o.TypeReadyForTask:	handleReadyForTask,
	o.TypeTaskResponse:	handleResult,
	/* C->P only messages, should never appear on the wire. */
	o.TypeTaskRequest:	handleIllegal,

}

var loopFudge int64 = 10e6; /* 10 ms should be enough fudgefactor */
func clientLogic(client *ClientInfo) {
	loop := true
	for loop {
		var	retryWait <-chan int64 = nil
		var	retryTask *o.TaskRequest = nil
		if (client.Player != "") {
			var waitTime int64 = 0
			var now int64 = 0
			cleanPass := false
			attempts := 0
			for !cleanPass && attempts < 10 {
				/* reset our state for the pass */
				waitTime = 0
				retryTask = nil
				attempts++
				cleanPass = true
				now = time.Nanoseconds() + loopFudge
				// if the client is correctly associated,
				// evaluate all jobs for outstanding retries,
				// and work out when our next retry is due.
				for _,v := range client.pendingTasks {
					if v.RetryTime < now {
						client.SendTask(v)
						cleanPass = false
					} else {
						if waitTime == 0 || v.RetryTime < waitTime {
							retryTask = v
							waitTime = v.RetryTime
						}
					}
				}
			}
			if (attempts > 10) {
				o.Fail("Couldn't find next timeout without restarting excessively.")
			}
			if (retryTask != nil) {
				retryWait = time.After(waitTime-time.Nanoseconds())
			}
		}
		select {
		case <-retryWait:
			client.SendTask(retryTask)
		case p := <-client.PktInQ:
			/* we've received a packet.  do something with it. */
			if client.Player == "" && p.Type != o.TypeIdentifyClient {
				o.Warn("Client %s didn't Identify self - got type %d instead!", client.Name(), p.Type)
				client.Abort()
				break
			}
			var upkt interface {} = nil
			if p.Length > 0 {
				var err os.Error

				upkt, err = p.Decode()
				if err != nil {
					o.Warn("Error unmarshalling message from Client %s: %s", client.Name(), err)
					client.Abort()
					break
				}
			}
			handler, exists := dispatcher[p.Type]
			if (exists) {
				handler(client, upkt)
			} else {
				o.Warn("Unhandled Pkt Type %d", p.Type)
			}
		case p := <-client.PktOutQ:
			if p != nil {
				_, err := p.Send(client.connection)
				if err != nil {
					o.Warn("Error sending pkt to %s: %s", client.Name(), err)
					client.Abort()
				}
			}
		case t := <-client.TaskQ:
			client.GotTask(t)
		case <-client.abortQ:
			o.Warn("Client %s connection writer on fire!", client.Name())
			loop = false
		case <-time.After(KeepaliveDelay):
			p := o.MakeNop()
			o.Warn("Sending Keepalive to %s", client.Name())
			_, err := p.Send(client.connection)
			if err != nil {
				o.Warn("Error sending pkt to %s: %s", client.Name(), err)	
				client.Abort()
			}
		}
	}
	client.connection.Close()
}

func clientReceiver(client *ClientInfo) {
	conn := client.connection


	loop := true
	for loop {
		pkt, err := o.Receive(conn)
		if nil != err {
			o.Warn("Error receiving pkt from %s: %s", conn.RemoteAddr().String(), err)
			client.Abort()
			client.connection.Close()
			loop = false
		} else {
			client.PktInQ <- pkt
		}
	}
	o.Warn("Client %s connection reader on fire!", conn.RemoteAddr().String())
}

/* The Main Server loop calls this method to hand off connections to us */
func HandleConnection(conn net.Conn) {
	/* this is a temporary client info, we substitute it for the real
	 * one once we ID the connection correctly */
	c := NewClientInfo()
	c.connection = conn
	go clientReceiver(c)
	go clientLogic(c)
}