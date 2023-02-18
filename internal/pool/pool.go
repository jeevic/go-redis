package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeevic/go-redis/v9/internal"
)

var (
	// ErrClosed performs any operation on the closed client will return this error.
	ErrClosed = errors.New("redis: client is closed")

	// ErrPoolTimeout timed out waiting to get a connection from the connection pool.
	ErrPoolTimeout = errors.New("redis: connection pool timeout")
)

var timers = sync.Pool{
	New: func() interface{} {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	},
}

// Stats contains pool state information and accumulated stats.
type Stats struct {
	Hits     uint32 // number of times free connection was found in the pool
	Misses   uint32 // number of times free connection was NOT found in the pool
	Timeouts uint32 // number of times a wait timeout occurred

	TotalConns uint32 // number of total connections in the pool
	IdleConns  uint32 // number of idle connections in the pool
	StaleConns uint32 // number of stale connections removed from the pool
}

type Pooler interface {
	NewConn(context.Context) (*Conn, error)
	CloseConn(*Conn) error

	Get(context.Context) (*Conn, error)
	Put(context.Context, *Conn)
	Remove(context.Context, *Conn, error)

	Len() int
	IdleLen() int
	Stats() *Stats

	Close() error
}

type Options struct {
	Dialer func(context.Context) (net.Conn, error)

	PoolFIFO        bool
	PoolSize        int
	PoolTimeout     time.Duration
	MinIdleConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}

type lastDialErrorWrap struct {
	err error
}

type ConnPool struct {
	cfg *Options

	dialErrorsNum uint32 // atomic
	lastDialError atomic.Value

	queue chan struct{}

	connsMu   sync.Mutex
	conns     *list.List
	idleConns *list.List

	poolSize     int
	idleConnsLen int

	stats Stats

	_closed uint32 // atomic
}

var _ Pooler = (*ConnPool)(nil)

func NewConnPool(opt *Options) *ConnPool {
	p := &ConnPool{
		cfg: opt,

		queue:     make(chan struct{}, opt.PoolSize),
		conns:     list.New(),
		idleConns: list.New(),
	}

	p.connsMu.Lock()
	p.checkMinIdleConns()
	p.connsMu.Unlock()

	return p
}

func (p *ConnPool) checkMinIdleConns() {
	if p.cfg.MinIdleConns == 0 {
		return
	}
	for p.poolSize < p.cfg.PoolSize && p.idleConnsLen < p.cfg.MinIdleConns {
		p.poolSize++
		p.idleConnsLen++
		go func() {
			err := p.addIdleConn()
			if err != nil && err != ErrClosed {
				p.connsMu.Lock()
				p.poolSize--
				p.idleConnsLen--
				p.connsMu.Unlock()
			}
		}()
	}
}

func (p *ConnPool) addIdleConn() error {
	cn, err := p.dialConn(context.TODO(), true)
	if err != nil {
		return err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return ErrClosed
	}

	p.conns.PushBack(cn)
	p.idleConns.PushBack(cn)
	return nil
}

func (p *ConnPool) NewConn(ctx context.Context) (*Conn, error) {
	return p.newConn(ctx, false)
}

func (p *ConnPool) newConn(ctx context.Context, pooled bool) (*Conn, error) {
	cn, err := p.dialConn(ctx, pooled)
	if err != nil {
		return nil, err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return nil, ErrClosed
	}

	p.conns.PushBack(cn)
	if pooled {
		// If pool is full remove the cn on next Put.
		if p.poolSize >= p.cfg.PoolSize {
			cn.pooled = false
		} else {
			p.poolSize++
		}
	}

	return cn, nil
}

func (p *ConnPool) dialConn(ctx context.Context, pooled bool) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if atomic.LoadUint32(&p.dialErrorsNum) >= uint32(p.cfg.PoolSize) {
		return nil, p.getLastDialError()
	}

	netConn, err := p.cfg.Dialer(ctx)
	if err != nil {
		p.setLastDialError(err)
		if atomic.AddUint32(&p.dialErrorsNum, 1) == uint32(p.cfg.PoolSize) {
			go p.tryDial()
		}
		return nil, err
	}

	cn := NewConn(netConn)
	cn.pooled = pooled
	return cn, nil
}

func (p *ConnPool) tryDial() {
	for {
		if p.closed() {
			return
		}

		conn, err := p.cfg.Dialer(context.Background())
		if err != nil {
			p.setLastDialError(err)
			time.Sleep(time.Second)
			continue
		}

		atomic.StoreUint32(&p.dialErrorsNum, 0)
		_ = conn.Close()
		return
	}
}

func (p *ConnPool) setLastDialError(err error) {
	p.lastDialError.Store(&lastDialErrorWrap{err: err})
}

func (p *ConnPool) getLastDialError() error {
	err, _ := p.lastDialError.Load().(*lastDialErrorWrap)
	if err != nil {
		return err.err
	}
	return nil
}

// Get returns existed connection from the pool or creates a new one.
func (p *ConnPool) Get(ctx context.Context) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if err := p.waitTurn(ctx); err != nil {
		return nil, err
	}

	var attempt int32 = 3
	var i int32 = 0

	for {
		p.connsMu.Lock()
		cn, err := p.popIdle()
		p.connsMu.Unlock()

		if err != nil {
			return nil, err
		}

		if cn == nil {
			//尝试三次 中断
			if atomic.AddInt32(&i, 1) >= attempt {
				break
			}
			//其他情况继续找
			continue
		}

		if !p.isHealthyConn(cn) {
			p.AsyncCloseConn(cn)
			continue
		}

		atomic.AddUint32(&p.stats.Hits, 1)
		return cn, nil
	}

	atomic.AddUint32(&p.stats.Misses, 1)
	newcn, err := p.newConn(ctx, true)
	if err != nil {
		p.freeTurn()
		return nil, err
	}

	return newcn, nil
}

func (p *ConnPool) getTurn() {
	p.queue <- struct{}{}
}

func (p *ConnPool) waitTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case p.queue <- struct{}{}:
		return nil
	default:
	}

	timer := timers.Get().(*time.Timer)
	timer.Reset(p.cfg.PoolTimeout)

	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return ctx.Err()
	case p.queue <- struct{}{}:
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return nil
	case <-timer.C:
		timers.Put(timer)
		atomic.AddUint32(&p.stats.Timeouts, 1)
		return ErrPoolTimeout
	}
}

func (p *ConnPool) freeTurn() {
	<-p.queue
}

func (p *ConnPool) popIdle() (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}
	n := p.idleConns.Len()
	if n == 0 {
		return nil, nil
	}

	var cn *Conn
	var el *list.Element
	if p.cfg.PoolFIFO {
		el = p.idleConns.Front()
	} else {
		el = p.idleConns.Back()
	}
	if el != nil {
		cn = el.Value.(*Conn)
		//移出元素
		p.idleConns.Remove(el)
		p.idleConnsLen--
	}
	p.checkMinIdleConns()
	return cn, nil
}

func (p *ConnPool) Put(ctx context.Context, cn *Conn) {
	if cn.rd.Buffered() > 0 {
		internal.Logger.Printf(ctx, "Conn has unread data")
		p.AsyncRemove(ctx, cn, BadConnError{})
		return
	}

	if !cn.pooled {
		p.AsyncRemove(ctx, cn, nil)
		return
	}

	var shouldCloseConn bool

	p.connsMu.Lock()

	if p.cfg.MaxIdleConns == 0 || p.idleConnsLen < p.cfg.MaxIdleConns {
		p.idleConns.PushBack(cn)
		p.idleConnsLen++
	} else {
		p.removeConn(cn)
		shouldCloseConn = true
	}

	p.connsMu.Unlock()

	p.freeTurn()

	if shouldCloseConn {
		go func(cn *Conn) {
			_ = p.closeConn(cn)
		}(cn)
	}
}

func (p *ConnPool) Remove(_ context.Context, cn *Conn, reason error) {
	p.removeConnWithLock(cn)
	p.freeTurn()
	_ = p.closeConn(cn)
}

func (p *ConnPool) AsyncRemove(_ context.Context, cn *Conn, reason error) {
	p.removeConnWithLock(cn)
	p.freeTurn()
	go func(cn *Conn) {
		_ = p.closeConn(cn)
	}(cn)
}

func (p *ConnPool) CloseConn(cn *Conn) error {
	p.removeConnWithLock(cn)
	atomic.AddUint32(&p.stats.StaleConns, 1)
	return p.closeConn(cn)
}

func (p *ConnPool) AsyncCloseConn(cn *Conn) {
	p.removeConnWithLock(cn)
	atomic.AddUint32(&p.stats.StaleConns, 1)
	go func(cn *Conn) {
		_ = p.closeConn(cn)
	}(cn)
	return
}

func (p *ConnPool) removeConnWithLock(cn *Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	p.removeConn(cn)
}

func (p *ConnPool) removeConn(cn *Conn) {
	var c *Conn
	for e := p.conns.Front(); e != nil; e = e.Next() {
		c = e.Value.(*Conn)
		if c == cn {
			p.conns.Remove(e)
			if cn.pooled {
				p.poolSize--
				p.checkMinIdleConns()
			}
			break
		}
	}
}

func (p *ConnPool) closeConn(cn *Conn) error {
	return cn.Close()
}

// Len returns total number of connections.
func (p *ConnPool) Len() int {
	p.connsMu.Lock()
	n := p.conns.Len()
	p.connsMu.Unlock()
	return n
}

// IdleLen returns number of idle connections.
func (p *ConnPool) IdleLen() int {
	p.connsMu.Lock()
	n := p.idleConnsLen
	p.connsMu.Unlock()
	return n
}

func (p *ConnPool) Stats() *Stats {
	return &Stats{
		Hits:     atomic.LoadUint32(&p.stats.Hits),
		Misses:   atomic.LoadUint32(&p.stats.Misses),
		Timeouts: atomic.LoadUint32(&p.stats.Timeouts),

		TotalConns: uint32(p.Len()),
		IdleConns:  uint32(p.IdleLen()),
		StaleConns: atomic.LoadUint32(&p.stats.StaleConns),
	}
}

func (p *ConnPool) closed() bool {
	return atomic.LoadUint32(&p._closed) == 1
}

func (p *ConnPool) Filter(fn func(*Conn) bool) error {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	var firstErr error

	var cn *Conn
	for e := p.conns.Front(); e != nil; e = e.Next() {
		cn = e.Value.(*Conn)
		if fn(cn) {
			if err := p.closeConn(cn); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *ConnPool) Close() error {
	if !atomic.CompareAndSwapUint32(&p._closed, 0, 1) {
		return ErrClosed
	}

	var firstErr error
	p.connsMu.Lock()
	var cn *Conn
	for e := p.conns.Front(); e != nil; e = e.Next() {
		cn = e.Value.(*Conn)
		if err := p.closeConn(cn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	p.poolSize = 0
	p.idleConns = nil
	p.idleConnsLen = 0
	p.connsMu.Unlock()

	return firstErr
}

func (p *ConnPool) isHealthyConn(cn *Conn) bool {
	now := time.Now()

	if p.cfg.ConnMaxLifetime > 0 && now.Sub(cn.createdAt) >= p.cfg.ConnMaxLifetime {
		fmt.Printf("redis check conn max life time now:%s use at:%s, %d\n", now.Format("2006-01-02 15:04:05"), cn.createdAt.Format("2006-01-02 15:04:05"), p.cfg.ConnMaxLifetime.Milliseconds())
		return false
	}
	if p.cfg.ConnMaxIdleTime > 0 && now.Sub(cn.UsedAt()) >= p.cfg.ConnMaxIdleTime {
		fmt.Printf("redis check conn max idle time now:%s create at:%s use at:%s, %d\n", now.Format("2006-01-02 15:04:05"), cn.createdAt.Format("2006-01-02 15:04:05"), cn.UsedAt().Format("2006-01-02 15:04:05"), p.cfg.ConnMaxIdleTime.Milliseconds())
		return false
	}

	if err := connCheck(cn.netConn); err != nil {
		fmt.Printf("redis check err:%s", err.Error())
		return false
	}

	cn.SetUsedAt(now)
	return true
}

func (p *ConnPool) isStaleConn(cn *Conn) bool {
	now := time.Now()

	if p.cfg.ConnMaxLifetime > 0 && now.Sub(cn.createdAt) >= p.cfg.ConnMaxLifetime {
		fmt.Printf("redis stale check conn max life time now:%s use at:%s, %d\n", now.Format("2006-01-02 15:04:05"), cn.createdAt.Format("2006-01-02 15:04:05"), p.cfg.ConnMaxLifetime.Milliseconds())
		return true
	}
	if p.cfg.ConnMaxIdleTime > 0 && now.Sub(cn.UsedAt()) >= p.cfg.ConnMaxIdleTime {
		fmt.Printf("redis stale  check conn max idle time now:%s created_at:%s use at:%s, %d\n", now.Format("2006-01-02 15:04:05"), cn.createdAt.Format("2006-01-02 15:04:05"), cn.UsedAt().Format("2006-01-02 15:04:05"), p.cfg.ConnMaxIdleTime.Milliseconds())
		return true
	}

	if err := connCheck(cn.netConn); err != nil {
		fmt.Printf("redis stale check  err:%s", err.Error())
		return true
	}

	return false
}

func (p *ConnPool) reaper(frequency time.Duration) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// It is possible that ticker and closedCh arrive together,
			// and select pseudo-randomly pick ticker case, we double
			// check here to prevent being executed after closed.
			if p.closed() {
				return
			}
			_, _ = p.ReapStaleConns()
		}
	}
}

func (p *ConnPool) ReapStaleConns() (int, error) {
	var n int
	for {
		p.getTurn()

		p.connsMu.Lock()
		cn := p.reapStaleConn()
		p.connsMu.Unlock()

		p.freeTurn()

		if cn != nil {
			_ = p.closeConn(cn)
			n++
		} else {
			break
		}
	}

	atomic.AddUint32(&p.stats.StaleConns, uint32(n))
	return n, nil
}

func (p *ConnPool) reapStaleConn() *Conn {
	if p.idleConns.Len() == 0 {
		return nil
	}

	el := p.idleConns.Front()
	cn := el.Value.(*Conn)
	if !p.isStaleConn(cn) {
		return nil
	}
	p.idleConns.Remove(el)
	p.idleConnsLen--
	p.removeConn(cn)
	return cn
}
