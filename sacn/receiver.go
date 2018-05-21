package sacn

import (
	"errors"
	"net"
	"time"

	"golang.org/x/net/ipv4"
)

/*
NewReceiverSocket creates a new unicast Receiversocket that is capable of listening on the given
interface (string is for binding). If the error is not nil, DO NOT receive on the channels
of the returned object! They will block and never be closed!
The network interface is used to join multicast groups. On some OSes (eg Windows) you have
to provide an interface for multicast to work. On others "nil" may be enough. If you dont want
to use multicast for receiving, just provide "nil".
*/
func NewReceiverSocket(bind string, ifi *net.Interface) (ReceiverSocket, error) {
	r := ReceiverSocket{}

	ServerConn, err := net.ListenPacket("udp4", bind+":5568")
	if err != nil {
		return r, err
	}
	r.multicastInterface = ifi
	r.socket = ipv4.NewPacketConn(ServerConn)
	r.activated = make(map[uint16]struct{})
	r.lastDatas = make(map[uint16]*lastData)
	r.DataChan = make(chan DataPacket)
	r.ErrChan = make(chan ReceiveError)
	r.stopListener = make(chan struct{})
	r.startListener()
	return r, nil
}

//the listener is responsible for listening on the UDP socket and parsing the incoming data.
//It dispatches the received packets to the corresponding handlers.
func (r *ReceiverSocket) startListener() {
	go func() {
		buf := make([]byte, 638)
	Loop:
		for {
			select {
			case <-r.stopListener:
				break Loop //break if we had a stop signal from the stopChannel
			default:
			}

			r.socket.SetDeadline(time.Now().Add(time.Millisecond * timeoutMs))
			n, _, addr, _ := r.socket.ReadFrom(buf) //n, ControlMessage, addr, err
			if addr == nil {                        //Check if we had a timeout
				//that means we did not receive a packet in 2,5s at all
				//so all handlers are getting a nil
				for _, univ := range r.GetAllActive() {
					go r.handle(univ, nil)
				}
			}
			p, err := NewDataPacketRaw(buf[0:n])
			if err != nil {
				continue //if the packet could not be parsed, just skip it
			}
			//send the packet to the responding handler and the other are getting nil
			if r.isActive(p.Universe()) {
				go r.handle(p.Universe(), &p)
			}
			for _, univ := range r.GetAllActive() {
				if univ != p.Universe() {
					go r.handle(univ, nil)
				}
			}
		}
		r.socket.Close() //close the channel, if the listener is finished
	}()
}

//this function handles the datapacket, which can be nil. universe is the universe, it should handle
func (r *ReceiverSocket) handle(universe uint16, p *DataPacket) {
	//a handler is called for every packet that has arrived. p may be nil,
	//if the packet has another universe than `universe`
	if p != nil && universe == p.Universe() && r.isActive(universe) {
		m := r.lastDatas[universe].sources
		updateSourcesMap(m, *p)
		tmp := getAllowedSources(m)

		//if the length of allowed sources is greater than 1, we have the situation of
		//multiple sources transmitting on the same priority, so we send sources exceeded to the errchan
		if len(tmp) > 1 {
			errToCh(universe, errors.New("sources exceeded"), r.ErrChan)
			return //skip all steps down
		}
		//if the source of this packet is in the allowed sources list, let this packet pass
		if _, ok := tmp[p.CID()]; ok {
			lastData := r.lastDatas[universe] //lastData is a pointer, so we can use it as reference
			//check the sequence
			if !checkSequ(lastData.lastSequ, p.Sequence()) {
				return //if the sequence is not good, discard this packet
			}
			lastData.lastSequ = p.Sequence()
			lastData.lastTime = time.Now()
			//check if the data was changed
			if !equalData(lastData.lastDMXdata, p.Data()) {
				r.DataChan <- *p
				//make a copy as lastData, otherwise it will be a reference
				lastData.lastDMXdata = append(make([]byte, 0), p.Data()...)
			}
		}
	} else if time.Since(r.lastDatas[universe].lastTime) > timeoutMs*time.Millisecond {
		//timeout of our universe, this if is needed, because we may receive packets from other
		//universes but we have to listen only for ours
		errToCh(universe, errors.New("timeout"), r.ErrChan)
	}
}

//Helping functions and structs for storing source information
type source struct {
	//store the last time this source occurs
	lastTime time.Time
	//store the last time this priority occurs
	lastTimeHighPrio time.Time
	//store the highest priority from this source that is currently sended out
	highestPrio byte
}

//updates the map according to current time and the given packet
func updateSourcesMap(m map[[16]byte]source, p DataPacket) {
	//go through the map and update the entries
	for key, value := range m {
		if key == p.CID() {
			//We have the entry that is the same source as the one from the packet
			//update time
			value.lastTime = time.Now()
			//Check if the priority has been changed
			if value.highestPrio < p.Priority() {
				//priority is increased
				value.highestPrio = p.Priority()
				value.lastTimeHighPrio = time.Now()
			} else if value.highestPrio == p.Priority() {
				//priority stays the same, so update the time
				value.lastTimeHighPrio = time.Now()
			} else if value.highestPrio > p.Priority() {
				//the stored priority is lower than the packet's one
				//check for timeout of the highest priority
				if time.Since(value.lastTimeHighPrio) > timeoutMs*time.Millisecond {
					//if the highest priority is timeouted decrease the current priority
					value.highestPrio = p.Priority()
					value.lastTimeHighPrio = time.Now()
				}
			}
		} else {
			//If the source timeouted, delete it
			if time.Since(value.lastTime) > timeoutMs*time.Millisecond {
				delete(m, key)
			}
		}
	}
	//check if the source is new
	_, ok := m[p.CID()]
	if !ok { //if the source is new create a new entry
		m[p.CID()] = source{
			lastTime:         time.Now(),
			lastTimeHighPrio: time.Now(),
			highestPrio:      p.Priority(),
		}
	}
}

func getAllowedSources(m map[[16]byte]source) map[[16]byte]struct{} {
	//filter for the highest priority
	highestPrio := byte(0)
	for _, value := range m {
		if value.highestPrio > highestPrio {
			highestPrio = value.highestPrio
		}
	}
	//now get all sources with the highest priority
	new := make(map[[16]byte]struct{})
	//and store them in this set
	for key, value := range m {
		if value.highestPrio == highestPrio {
			new[key] = struct{}{}
		}
	}
	return new
}

func checkSequ(old, new byte) bool {
	//calculate in int
	tmp := int(new) - int(old)
	if tmp <= 0 && tmp > -20 {
		return false
	}
	return true
}
