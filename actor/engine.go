package actor

import (
	"sync"
	"time"

	"github.com/anthdm/hollywood/log"
)

type Remoter interface {
	Address() string
	Send(*PID, any, *PID)
	Start()
}

// Producer is any function that can return a Receiver
type Producer func() Receiver

// Receiver is an interface that can receive and process messages.
type Receiver interface {
	Receive(*Context)
}

// Engine represents the actor engine.
type Engine struct {
	EventStream *EventStream
	Registry    *Registry

	address    string
	remote     Remoter
	deadLetter Processer
	logger     log.Logger
}

// NewEngine returns a new actor Engine.
// You can pass an optional logger through
func NewEngine(opts ...func(*Engine)) *Engine {
	e := &Engine{}
	for _, o := range opts {
		o(e)
	}
	e.EventStream = NewEventStream(e.logger)
	e.address = LocalLookupAddr
	e.Registry = newRegistry(e)
	e.deadLetter = newDeadLetter(e.EventStream)
	e.Registry.add(e.deadLetter)
	return e
}

func EngineOptLogger(logger log.Logger) func(*Engine) {
	return func(e *Engine) {
		e.logger = logger
	}
}

func EngineOptPidSeparator(sep string) func(*Engine) {
	// This looks weird because the separator is a global variable.
	return func(e *Engine) {
		pidSeparator = sep
	}
}

// WithRemote returns a new actor Engine with the given Remoter,
// and will call its Start function
func (e *Engine) WithRemote(r Remoter) {
	e.remote = r
	e.address = r.Address()
	r.Start()
}

// Spawn spawns a process that will producer by the given Producer and
// can be configured with the given opts.
func (e *Engine) Spawn(p Producer, name string, opts ...OptFunc) *PID {
	options := DefaultOpts(p)
	options.Name = name
	for _, opt := range opts {
		opt(&options)
	}
	proc := newProcess(e, options)
	return e.SpawnProc(proc)
}

func (e *Engine) SpawnFunc(f func(*Context), id string, opts ...OptFunc) *PID {
	return e.Spawn(newFuncReceiver(f), id, opts...)
}

// SpawnProc spawns the give Processer. This function is useful when working
// with custom created Processes. Take a look at the streamWriter as an example.
func (e *Engine) SpawnProc(p Processer) *PID {
	e.Registry.add(p)
	p.Start()
	return p.PID()
}

// Address returns the address of the actor engine. When there is
// no remote configured, the "local" address will be used, otherwise
// the listen address of the remote.
func (e *Engine) Address() string {
	return e.address
}

// Request sends the given message to the given PID as a "Request", returning
// a response that will resolve in the future. Calling Response.Result() will
// block until the deadline is exceeded or the response is being resolved.
func (e *Engine) Request(pid *PID, msg any, timeout time.Duration) *Response {
	resp := NewResponse(e, timeout)
	e.Registry.add(resp)

	e.SendWithSender(pid, msg, resp.PID())

	return resp
}

// SendWithSender will send the given message to the given PID with the
// given sender. Receivers receiving this message can check the sender
// by calling Context.Sender().
func (e *Engine) SendWithSender(pid *PID, msg any, sender *PID) {
	e.send(pid, msg, sender)
}

// Send sends the given message to the given PID. If the message cannot be
// delivered due to the fact that the given process is not registered.
// The message will be send to the DeadLetter process instead.
func (e *Engine) Send(pid *PID, msg any) {
	e.send(pid, msg, nil)
}

func (e *Engine) send(pid *PID, msg any, sender *PID) {
	if e.isLocalMessage(pid) {
		e.SendLocal(pid, msg, sender)
		return
	}
	if e.remote == nil {
		e.logger.Errorw("failed sending messsage",
			"err", "engine has no remote configured")
		return
	}
	e.remote.Send(pid, msg, sender)
}

type SendRepeater struct {
	engine   *Engine
	self     *PID
	target   *PID
	msg      any
	interval time.Duration
	cancelch chan struct{}
}

func (sr SendRepeater) start() {
	ticker := time.NewTicker(sr.interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				sr.engine.SendWithSender(sr.target, sr.msg, sr.self)
			case <-sr.cancelch:
				ticker.Stop()
				return
			}
		}
	}()
}

func (sr SendRepeater) Stop() {
	close(sr.cancelch)
}

// SendRepeat will send the given message to the given PID each given interval.
// It will return a SendRepeater struct that can stop the repeating message by calling Stop().
func (e *Engine) SendRepeat(pid *PID, msg any, interval time.Duration) SendRepeater {
	clonedPID := *pid.CloneVT()
	sr := SendRepeater{
		engine:   e,
		self:     nil,
		target:   &clonedPID,
		interval: interval,
		msg:      msg,
		cancelch: make(chan struct{}, 1),
	}
	sr.start()
	return sr
}

// Stop will send a non-graceful poisonPill message to the process that is associated with the given PID.
// The process will shut down immediately, once it has processed the poisonPill messsage.
// If given a WaitGroup, it blocks till the process is completely shutdown.
func (e *Engine) Stop(pid *PID, wg ...*sync.WaitGroup) *sync.WaitGroup {
	return e.sendPoisonPill(pid, false, wg...)
}

// Poison will send a graceful poisonPill message to the process that is associated with the given PID.
// The process will shut down gracefully once it has processed all the messages in the inbox.
// If given a WaitGroup, it blocks till the process is completely shutdown.
func (e *Engine) Poison(pid *PID, wg ...*sync.WaitGroup) *sync.WaitGroup {
	return e.sendPoisonPill(pid, true, wg...)
}

func (e *Engine) sendPoisonPill(pid *PID, graceful bool, wg ...*sync.WaitGroup) *sync.WaitGroup {
	var _wg *sync.WaitGroup
	if len(wg) > 0 {
		_wg = wg[0]
	} else {
		_wg = &sync.WaitGroup{}
	}
	_wg.Add(1)
	proc := e.Registry.get(pid)
	pill := poisonPill{
		wg:       _wg,
		graceful: graceful,
	}
	if proc != nil {
		e.SendLocal(pid, pill, nil)
	}
	return _wg
}

func (e *Engine) SendLocal(pid *PID, msg any, sender *PID) {
	proc := e.Registry.get(pid)
	if proc != nil {
		proc.Send(pid, msg, sender)
	}
}

func (e *Engine) isLocalMessage(pid *PID) bool {
	return e.address == pid.Address
}

type funcReceiver struct {
	f func(*Context)
}

func newFuncReceiver(f func(*Context)) Producer {
	return func() Receiver {
		return &funcReceiver{
			f: f,
		}
	}
}

func (r *funcReceiver) Receive(c *Context) {
	r.f(c)
}
