package tunnel

import (
	"encoding/binary"
	"fmt"
	ex "github.com/Lafeng/deblocus/exception"
	log "github.com/Lafeng/deblocus/golang/glog"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// frame action 8bit
	FRAME_ACTION_CLOSE = iota
	FRAME_ACTION_CLOSE_R
	FRAME_ACTION_CLOSE_W
	FRAME_ACTION_OPEN
	FRAME_ACTION_OPEN_N
	FRAME_ACTION_OPEN_Y
	FRAME_ACTION_DATA
	FRAME_ACTION_PING
	FRAME_ACTION_PONG
	FRAME_ACTION_TOKENS
	FRAME_ACTION_TOKEN_REQUEST
	FRAME_ACTION_TOKEN_REPLY
	FRAME_ACTION_SLOWDOWN = 0xff
)

const (
	FAST_OPEN              = true
	FAST_OPEN_BUF_MAX_SIZE = 1 << 13 // 8k
)

const (
	WAITING_OPEN_TIMEOUT = GENERAL_SO_TIMEOUT * 3
	FRAME_HEADER_LEN     = 5
	FRAME_MAX_LEN        = 0xffff - FRAME_HEADER_LEN
	MUX_PENDING_CLOSE    = -1
	MUX_CLOSED           = -2
)

const (
	// idle error type
	ERR_PING_TIMEOUT = 0xe
	ERR_NEW_PING     = 0xf
	ERR_UNKNOWN      = 0x0
)

// --------------------
// event_handler
// --------------------
type event byte

var (
	evt_tokens = event(1)
)

type event_handler func(e event, msg ...interface{})

var (
	SID_SEQ uint32
	seqLock sync.Locker = new(sync.Mutex)
)

// --------------------
// idler
// --------------------
type idler struct {
	enabled  bool
	waiting  bool
	interval time.Duration
}

func NewIdler(interval int, isClient bool) *idler {
	if interval > 0 && (interval > 300 || interval < 30) {
		interval = DT_PING_INTERVAL
	}
	i := &idler{
		interval: time.Second * time.Duration(interval),
		enabled:  interval > 0,
	}
	if isClient {
		i.interval -= GENERAL_SO_TIMEOUT
	} else { // server ping will be behind
		i.interval += GENERAL_SO_TIMEOUT
	}
	i.interval += time.Duration(randomRange(int64(time.Second), int64(GENERAL_SO_TIMEOUT)))
	return i
}

func (i *idler) newRound(tun *Conn) {
	if i.enabled {
		if i.waiting { // ping sent, waiting response
			tun.SetReadDeadline(time.Now().Add(GENERAL_SO_TIMEOUT))
		} else {
			tun.SetReadDeadline(time.Now().Add(i.interval))
		}
	} /* else {
		tun.SetReadDeadline(ZERO_TIME)
	} */
}

func (i *idler) consumeError(er error) uint {
	if i.enabled {
		if netErr, y := er.(net.Error); y && netErr.Timeout() {
			if i.waiting {
				return ERR_PING_TIMEOUT
			} else {
				return ERR_NEW_PING
			}
		}
	}
	return ERR_UNKNOWN
}

func (i *idler) ping(tun *Conn) error {
	if i.enabled {
		i.waiting = true
		buf := make([]byte, FRAME_HEADER_LEN)
		_frame(buf, FRAME_ACTION_PING, 0, nil)
		return tunWrite1(tun, buf)
	}
	return nil
}

func (i *idler) pong(tun *Conn) error {
	if i.enabled {
		buf := make([]byte, FRAME_HEADER_LEN)
		_frame(buf, FRAME_ACTION_PONG, 0, nil)
		return tunWrite1(tun, buf)
	}
	return nil
}

func (i *idler) verify() (r bool) {
	r = i.waiting
	if i.waiting {
		i.waiting = false
	}
	return
}

// --------------------
// frame
// --------------------
type frame struct {
	action uint8
	sid    uint16
	length uint16
	data   []byte
	conn   *edgeConn
}

func (f *frame) String() string {
	return fmt.Sprintf("Frame{sid=%d act=%d len=%d}", f.sid, f.action, f.length)
}

func (f *frame) toNewBuffer() []byte {
	b := make([]byte, f.length+FRAME_HEADER_LEN)
	b[0] = f.action
	binary.BigEndian.PutUint16(b[1:], f.sid)
	binary.BigEndian.PutUint16(b[3:], f.length)
	if f.length > 0 {
		copy(b[FRAME_HEADER_LEN:], f.data)
	}
	return b
}

// --------------------
// multiplexer
// --------------------
type multiplexer struct {
	isClient bool
	pool     *ConnPool
	router   *egressRouter
	role     string
	status   int
	pingCnt  int32 // received ping count
}

func newServerMultiplexer() *multiplexer {
	m := &multiplexer{
		isClient: false,
		pool:     NewConnPool(),
		role:     "SVR",
	}
	m.router = newEgressRouter(m)
	return m
}

func newClientMultiplexer() *multiplexer {
	m := &multiplexer{
		isClient: true,
		pool:     NewConnPool(),
		role:     "CLT",
	}
	m.router = newEgressRouter(m)
	return m
}

// destroy each listener of all pooled tun, and destroy egress queues
func (p *multiplexer) destroy() {
	if p.status < 0 {
		return
	}
	defer func() {
		if !ex.CatchException(recover()) {
			p.status = MUX_CLOSED
		}
	}()
	// will not send evt_dt_closed while pending_close was indicated
	p.status = MUX_PENDING_CLOSE
	p.router.destroy() // destroy queue
	p.pool.destroy()
	p.router = nil
	p.pool = nil
}

func (p *multiplexer) HandleRequest(prot string, client net.Conn, target string) {
	sid := _nextSID()
	if log.V(1) {
		log.Infof("%s->[%s] from=%s sid=%d\n", prot, target, ipAddr(client.RemoteAddr()), sid)
	}
	tun := p.pool.Select()
	ThrowIf(tun == nil, "No tun to deliveries request")
	key := sessionKey(tun, sid)
	edge := p.router.register(key, target, tun, client, true) // write edge
	p.relay(edge, tun, sid)                                   // read edge
}

func (p *multiplexer) onTunDisconnected(tun *Conn, handler event_handler) {
	if p.router != nil {
		p.router.cleanOfTun(tun)
	}
	if p.pool != nil {
		p.pool.Remove(tun)
	}
	SafeClose(tun)
}

// TODO notify peer to slow down when queue increased too fast
func (p *multiplexer) Listen(tun *Conn, handler event_handler, interval int) {
	tun.priority = &TSPriority{0, 1e9}
	p.pool.Push(tun)
	defer p.onTunDisconnected(tun, handler)
	tun.SetSockOpt(1, 2, 1)
	var (
		header = make([]byte, FRAME_HEADER_LEN)
		router = p.router
		idle   = NewIdler(interval, p.isClient)
		nr     int
		er     error
		frm    *frame
		key    string
	)
	if !p.isClient {
		// first ping frame will let client to be aware of using a valid token.
		idle.ping(tun)
	}
	for {
		idle.newRound(tun)
		nr, er = io.ReadFull(tun, header)
		if nr == FRAME_HEADER_LEN {
			frm = _parseFrameHeader(header)
			if frm.length > 0 {
				nr, er = io.ReadFull(tun, frm.data)
			}
		}
		if er != nil {
			switch idle.consumeError(er) {
			case ERR_NEW_PING:
				if idle.ping(tun) == nil {
					continue
				}
			case ERR_PING_TIMEOUT:
				if log.V(2) {
					log.Warningln("Peer was unresponsive then close", tun.identifier)
				}
			default:
				if log.V(2) {
					log.Warningln("Read tunnel", tun.identifier, er)
				}
			}
			return // error, abandon tunnel
		}
		key = sessionKey(tun, frm.sid)

		switch frm.action {
		case FRAME_ACTION_CLOSE_W:
			if edge, _ := router.getRegistered(key); edge != nil {
				edge.bitwiseCompareAndSet(TCP_CLOSE_W)
				edge.deliver(frm)
			}
		case FRAME_ACTION_CLOSE_R:
			if edge, _ := router.getRegistered(key); edge != nil {
				edge.bitwiseCompareAndSet(TCP_CLOSE_R)
				closeR(edge.conn)
			}
		case FRAME_ACTION_DATA:
			edge, pre := router.getRegistered(key)
			if edge != nil {
				edge.deliver(frm)
			} else if pre {
				router.preDeliver(key, frm)
			} else {
				if log.V(2) {
					log.Warningln("peer send data to an unexisted socket.", key, frm)
				}
				// trigger sending close to notice peer.
				_frame(header, FRAME_ACTION_CLOSE_R, frm.sid, nil)
				if tunWrite1(tun, header) != nil {
					return
				}
			}
		case FRAME_ACTION_OPEN:
			router.preRegister(key)
			go p.connectToDest(frm, key, tun)
		case FRAME_ACTION_OPEN_N, FRAME_ACTION_OPEN_Y:
			edge, _ := router.getRegistered(key)
			if edge != nil {
				if log.V(4) {
					if frm.action == FRAME_ACTION_OPEN_Y {
						log.Infoln(p.role, "recv OPEN_Y", frm)
					} else {
						log.Infoln(p.role, "recv OPEN_N", frm)
					}
				}
				edge.ready <- frm.action
				close(edge.ready)
			} else {
				if log.V(2) {
					log.Warningln("peer send OPENx to an unexisted socket.", key, frm)
				}
			}
		case FRAME_ACTION_PING:
			if idle.pong(tun) == nil {
				atomic.AddInt32(&p.pingCnt, 1)
			} else { // reply pong failed
				return
			}
		case FRAME_ACTION_PONG:
			if !idle.verify() {
				log.Warningln("Incorrect action_pong received")
			}
		case FRAME_ACTION_TOKENS:
			handler(evt_tokens, frm.data)
		default:
			log.Errorln(p.role, "Unrecognized", frm)
		}
		tun.Update()
	}
}

func sessionKey(tun *Conn, sid uint16) string {
	if tun.identifier != NULL {
		return tun.identifier + "." + strconv.FormatUint(uint64(sid), 10)
	} else {
		return fmt.Sprintf("%s_%s_%d", tun.LocalAddr(), tun.RemoteAddr(), sid)
	}
}

func (p *multiplexer) connectToDest(frm *frame, key string, tun *Conn) {
	defer func() {
		ex.CatchException(recover())
	}()
	var (
		dstConn net.Conn
		err     error
		target  = string(frm.data)
	)
	dstConn, err = net.DialTimeout("tcp", target, GENERAL_SO_TIMEOUT)
	frm.length = 0
	if err != nil {
		p.router.removePreRegistered(key)
		log.Errorf("Cannot connect to [%s] for %s error: %s\n", target, key, err)
		frm.action = FRAME_ACTION_OPEN_N
		tunWrite2(tun, frm)
	} else {
		if log.V(1) {
			log.Infoln("OPEN", target, "for", key)
		}
		dstConn.SetReadDeadline(ZERO_TIME)
		edge := p.router.register(key, target, tun, dstConn, false) // write edge
		frm.action = FRAME_ACTION_OPEN_Y
		if tunWrite2(tun, frm) == nil {
			p.relay(edge, tun, frm.sid) // read edge
		} else { // send open_y failed
			SafeClose(tun)
		}
	}
}

func (p *multiplexer) relay(edge *edgeConn, tun *Conn, sid uint16) {
	var (
		buf  = make([]byte, FRAME_MAX_LEN)
		code byte
		src  = edge.conn
	)
	defer func() {
		if edge.bitwiseCompareAndSet(TCP_CLOSE_R) { // only positively
			_frame(buf, FRAME_ACTION_CLOSE_W, sid, nil)
			tunWrite1(tun, buf[:FRAME_HEADER_LEN]) // tell peer to closeW
		}
		if code == FRAME_ACTION_OPEN_Y {
			closeR(src)
		} else { // remote open failed
			SafeClose(src)
		}
	}()
	if edge.positive { // for client
		_len := _frame(buf, FRAME_ACTION_OPEN, sid, []byte(edge.dest))
		if tunWrite1(tun, buf[:_len]) != nil {
			SafeClose(tun)
			return
		}
	}

	var (
		tn         int // total
		nr         int
		er         error
		_fast_open = /* FAST_OPEN && */ p.isClient
	)
	for {
		if _fast_open {
			v, y := reflect.ValueOf(edge.ready).TryRecv()
			if y {
				code = v.Interface().(byte)
				switch code {
				case FRAME_ACTION_OPEN_Y:
					_fast_open = false // read forever
				case FRAME_ACTION_OPEN_N:
					return
				}
			} else {
				if tn >= FAST_OPEN_BUF_MAX_SIZE { // must waiting for signal
					select {
					case code = <-edge.ready:
					case <-time.After(WAITING_OPEN_TIMEOUT):
						log.Errorf("waiting open-signal sid=%d timeout for %s\n", sid, edge.dest)
					}
					// timeout or open-signal received
					if code == FRAME_ACTION_OPEN_Y {
						_fast_open = false // read forever
					} else {
						return
					}
				} // else could read again
			}
		}

		nr, er = src.Read(buf[FRAME_HEADER_LEN:])
		if nr > 0 {
			tn += nr
			_frame(buf, FRAME_ACTION_DATA, sid, uint16(nr))
			if tunWrite1(tun, buf[:nr+FRAME_HEADER_LEN]) != nil {
				SafeClose(tun)
				return
			}
		}
		if er != nil {
			return
		}
	}
}

func (p *multiplexer) bestSend(data []byte, action_desc string) bool {
	var buf = make([]byte, FRAME_HEADER_LEN+len(data))
	_frame(buf, FRAME_ACTION_TOKENS, 0, data)
	var tun *Conn
	for i := 1; i <= 3; i++ {
		if p.status < 0 /* MUX_CLOSED */ || p.pool == nil {
			if log.V(4) {
				log.Warningln("abandon sending data of", action_desc)
			}
			break
		}
		tun = p.pool.Select()
		if tun != nil {
			if tunWrite1(tun, buf) == nil {
				return true
			}
		} else {
			time.Sleep(time.Millisecond * 200 * time.Duration(i))
		}
	}
	if log.V(3) {
		log.Warningln("failed to send data of", action_desc)
	}
	return false
}

func tunWrite1(tun *Conn, buf []byte) (err error) {
	err = tun.SetWriteDeadline(time.Now().Add(GENERAL_SO_TIMEOUT * 2))
	if err != nil {
		return
	}
	var nr, nw int
	nr = len(buf)
	nw, err = tun.Write(buf)
	if nr != nw || err != nil {
		log.Warningf("Write tun(%s) error(%v) when sending buf.len=%d\n", tun.sign(), err, nr)
		SafeClose(tun)
		return
	}
	return nil
}

func tunWrite2(tun *Conn, frm *frame) (err error) {
	err = tun.SetWriteDeadline(time.Now().Add(GENERAL_SO_TIMEOUT * 2))
	if err != nil {
		return
	}
	var nr, nw int
	nr = int(frm.length) + FRAME_HEADER_LEN
	nw, err = tun.Write(frm.toNewBuffer())
	if nr != nw || err != nil {
		log.Warningf("Write tun(%s) error(%v) when sending %s\n", tun.sign(), err, frm)
		SafeClose(tun)
		return
	}
	return nil
}

func _nextSID() uint16 {
	seqLock.Lock()
	defer seqLock.Unlock()
	SID_SEQ += 1
	if SID_SEQ > 0xffff {
		SID_SEQ = 1
	}
	return uint16(SID_SEQ)
}

func _parseFrameHeader(header []byte) *frame {
	f := &frame{
		header[0],
		binary.BigEndian.Uint16(header[1:]),
		binary.BigEndian.Uint16(header[3:]),
		nil, nil,
	}
	if f.length > 0 {
		f.data = make([]byte, f.length)
	}
	return f
}

func _frame(buf []byte, action byte, sid uint16, body_or_len interface{}) int {
	var _len = FRAME_HEADER_LEN
	buf[0] = action
	binary.BigEndian.PutUint16(buf[1:], sid)
	if body_or_len != nil {
		switch body_or_len.(type) {
		case []byte:
			body := body_or_len.([]byte)
			_len += len(body)
			binary.BigEndian.PutUint16(buf[3:], uint16(len(body)))
			copy(buf[FRAME_HEADER_LEN:], body)
		case uint16:
			blen := body_or_len.(uint16)
			_len += int(blen)
			binary.BigEndian.PutUint16(buf[3:], blen)
		default:
			panic("unknown body_or_len")
		}
	} else {
		buf[3] = 0
		buf[4] = 0
	}
	return _len
}
