// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancellation signals, and other request-scoped values across API boundaries
// and between processes.
//
// Incoming requests to a server should create a Context, and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using WithCancel, WithDeadline,
// WithTimeout, or WithValue. When a Context is canceled, all
// Contexts derived from it are also canceled.
//
// The WithCancel, WithDeadline, and WithTimeout functions take a
// Context (the parent) and return a derived Context (the child) and a
// CancelFunc. Calling the CancelFunc cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled or the timer
// fires. The go vet tool checks that CancelFuncs are used on all
// control-flow paths.
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. The Context should be the first
// parameter, typically named ctx:
//
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
//
// Do not pass a nil Context, even if a function permits it. Pass context.TODO
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
//
// See https://blog.golang.org/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// A Context carries a deadline, a cancellation signal, and other values across
// API boundaries.
//
// Context's methods may be called by multiple goroutines simultaneously.
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled. Deadline returns ok==false when no deadline is
	// set. Successive calls to Deadline return the same results.
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled. Done may return nil if this context can
	// never be canceled. Successive calls to Done return the same value.
	// The close of the Done channel may happen asynchronously,
	// after the cancel function returns.
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	//  // Stream generates values with DoSomething and sends them to out
	//  // until DoSomething returns an error or ctx.Done is closed.
	//  func Stream(ctx context.Context, out chan<- Value) error {
	//  	for {
	//  		v, err := DoSomething(ctx)
	//  		if err != nil {
	//  			return err
	//  		}
	//  		select {
	//  		case <-ctx.Done():
	//  			return ctx.Err()
	//  		case out <- v:
	//  		}
	//  	}
	//  }
	//
	// See https://blog.golang.org/pipelines for more examples of how to use
	// a Done channel for cancellation.
	Done() <-chan struct{}

	// If Done is not yet closed, Err returns nil.
	// If Done is closed, Err returns a non-nil error explaining why:
	// Canceled if the context was canceled
	// or DeadlineExceeded if the context's deadline passed.
	// After Err returns a non-nil error, successive calls to Err return the same error.
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key. Successive calls to Value with
	// the same key returns the same result.
	//
	// Use context values only for request-scoped data that transits
	// processes and API boundaries, not for passing optional parameters to
	// functions.
	//
	// A key identifies a specific value in a Context. Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value. A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stored using that key:
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts. It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	Value(key interface{}) interface{}
}

// Canceled is the error returned by Context.Err when the context is canceled.
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// An emptyCtx is never canceled, has no values, and has no deadline. It is not
// struct{}, since vars of this type must have distinct addresses.
type emptyCtx int

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*emptyCtx) Done() <-chan struct{} {
	return nil
}

func (*emptyCtx) Err() error {
	return nil
}

func (*emptyCtx) Value(key interface{}) interface{} {
	return nil
}

func (e *emptyCtx) String() string {
	switch e {
	case background:
		return "context.Background"
	case todo:
		return "context.TODO"
	}
	return "unknown empty Context"
}

var (
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline. It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming
// requests.
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context. Code should use context.TODO when
// it's unclear which Context to use or it is not yet available (because the
// surrounding function has not yet been extended to accept a Context
// parameter).
func TODO() Context {
	return todo
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// A CancelFunc may be called by multiple goroutines simultaneously.
// After the first call, subsequent calls to a CancelFunc do nothing.
type CancelFunc func()

// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := newCancelCtx(parent)   //将传入的上下文包装成私有结构体 context.cancelCtx
	propagateCancel(parent, &c) //会构建父子上下文之间的关联，当父上下文被取消时，子上下文也会被取消
	return &c, func() { c.cancel(true, Canceled) }
}

// newCancelCtx returns an initialized cancelCtx.
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

//记录当前已经创建的goroutine数量
// goroutines counts the number of goroutines ever created; for testing.
var goroutines int32

// propagateCancel arranges for child to be canceled when parent is.
//构建父子上下文之间的关联，当父上下文被取消时，子上下文也会被取消
//在 parent 和 child 之间同步取消和结束的信号，保证在 parent 被取消时，child 也会收到对应的信号，不会发生状态不一致的问题
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil { //当 parent.Done() == nil，也就是 parent 不会触发取消事件时，当前函数会直接返回；（说明父上下文根本就没有结束这个操作（不是调用 context.WithCancel或者context.WithDeadline生成的，没有对应的cancel方法）（从语义上 done表示context完成或者取消，如果done为nil，说明外部根本就没有办法通过done感知到 context是否结束)，这样的话，就不用构建 父子上下文之间的关联了（反正父上下文也不会被取消，所以子上下文 只有在自己的cancel方法被执行或者过期的时候 才会被cancel））
		return // parent is never canceled
	}

	select {
	case <-done: // 父上下文已经被取消，则
		// parent is already canceled
		child.cancel(false, parent.Err()) //取消子上下文，并向所有的子上下文同步取消信号
		return
	default:
	}

	if p, ok := parentCancelCtx(parent); ok { //获取 父context对应的 cancelCtx结构体；如果有（内部实现其实是父context不是 cancelCtx结构体）
		p.mu.Lock()
		if p.err != nil { //如果父context已经取消，则直接取消 子context
			// parent has already been canceled
			child.cancel(false, p.err) //取消子上下文
		} else {
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{} //c否则child 会被加入 parent 的 children 列表中，等待 parent 释放取消信号
		}
		p.mu.Unlock()
	} else { //如果父上下文没有对应的cancelCtx结构体（内部实现其实是父context是 cancelCtx结构体，其实就是没有cancel方法）
		atomic.AddInt32(&goroutines, +1) //运行一个新的 Goroutine 同时监听 parent.Done() 和 child.Done() 两个 Channel
		go func() {
			select {
			case <-parent.Done(): //监听parent.Done()，接收到 done消息则 cancel掉子context
				child.cancel(false, parent.Err()) //在 parent.Done() 关闭时调用 child.cancel 取消子上下文
			case <-child.Done(): // 监听 子context.Done()的消息，如果done 也直接返回
			}
		}()
	}
}

// &cancelCtxKey is the key that a cancelCtx returns itself for.
//作为cancelCtx中的一个key，trick：使用地址 防止和其他key重复
var cancelCtxKey int

// parentCancelCtx returns the underlying *cancelCtx for parent.
// It does this by looking up parent.Value(&cancelCtxKey) to find
// the innermost enclosing *cancelCtx and then checking whether
// parent.Done() matches that *cancelCtx. (If not, the *cancelCtx
// has been wrapped in a custom implementation providing a
// different done channel, in which case we should not bypass it.)
/*
parentCancelCtx返回parent对应的cancelCtx(这个结构体中封装了context的cancel逻辑)。
查找逻辑：通过调用parent.Value（＆cancelCtxKey）来查找parent对应的的 cancelCtx，
如果 parent已经被取消，则返回 nil，false
*/
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()
	if done == closedchan || done == nil { //如果是context.Backgroud 或者context.Todo（其实就是parentContext 不能被取消，直接返回 ）
		return nil, false
	}
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx) //如果 parentContext 没有 cancelCtx（内部实现应该是 父context不是一个 调用witchcancel或者withdeadline 生成的 context（调用witchcancel或者withdeadline生成的context都是 *cancelCtx类型，调用*cancelCtx.value(&cancelCtxKey)方法返回的是*cancelCtx本身 ）），直接返回
	if !ok {
		return nil, false
	}
	p.mu.Lock()
	ok = p.done == done
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	return p, true
}

// removeChild removes a context from its parent.
//将context从 他们parent的cancelCtx中child元素中移除
func removeChild(parent Context, child canceler) {
	//parentCancelCtx返回parent对应的cancelCtx
	p, ok := parentCancelCtx(parent)
	if !ok { //如果parent 没有 cancelCtx，直接返回
		return
	}
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child) //将 child 从 parent的children字段移除
	}
	p.mu.Unlock()
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// closedchan is a reusable closed channel.
var closedchan = make(chan struct{})

func init() {
	close(closedchan)
}

// A cancelCtx can be canceled. When canceled, it also cancels any children
// that implement canceler.
type cancelCtx struct {
	Context //父上下文 的 复制

	mu       sync.Mutex            // protects following fields
	done     chan struct{}         // created lazily, closed by first cancel call 懒加载，第一次调用cancel函数或者Done()的时候 才会被赋值
	children map[canceler]struct{} // set to nil by the first cancel call 子cancel context列表
	err      error                 // set to non-nil by the first cancel call cancel原因，如果不为nil，则表示已经被cancel
}

func (c *cancelCtx) Value(key interface{}) interface{} {
	if key == &cancelCtxKey {
		return c
	}
	return c.Context.Value(key)
}

func (c *cancelCtx) Done() <-chan struct{} {
	c.mu.Lock()
	if c.done == nil {
		c.done = make(chan struct{})
	}
	d := c.done
	c.mu.Unlock()
	return d
}

func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
// 关闭上下文中的 Channel 并向所有的子上下文同步取消信号
//如果 removeFromParent为true，则将 c从他的parent的chilren列表中移除
func (c *cancelCtx) cancel(removeFromParent bool, err error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled
	}
	c.err = err
	if c.done == nil { //关闭done channel
		c.done = closedchan
	} else {
		close(c.done)
	}
	for child := range c.children { //将子 context全部关闭
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent { //如果从parent context移除的话
		removeChild(c.Context, c)
	}
}

// WithDeadline returns a copy of the parent context with the deadline adjusted
// to be no later than d. If the parent's deadline is already earlier than d,
// WithDeadline(parent, d) is semantically equivalent to parent. The returned
// context's Done channel is closed when the deadline expires, when the returned
// cancel function is called, or when the parent context's Done channel is
// closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
//WithDeadline在创建 context.timerCtx 的过程中，判断了父上下文的截止日期与当前日期，
//并通过 time.AfterFunc 创建定时器，当时间超过了截止日期后会调用 context.timerCtx.cancel 方法同步取消信号
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if cur, ok := parent.Deadline(); /*获取 父上下文的截止时间*/ ok && cur.Before(d) { //如果父上下文设置了截止时间，并且 父上下文的截止时间 要早于 当前设置的截止时间，则直接调用 WithCancel(parent)并返回(通过 父上下文 截止时间到了以后的取消事件 来触发 子上下文的取消事件)
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	//建立父子之间的级联关系
	propagateCancel(parent, c)
	dur := time.Until(d) //计算当前时间到 截止时间之间的 间隔
	if dur <= 0 {        //如果已经过了截止时间
		c.cancel(true, DeadlineExceeded)               // deadline has already passed 直接cancel当前的上下文
		return c, func() { c.cancel(false, Canceled) } //返回已经被cancel的上下文
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded) //定时cancel上下文
		})
	}
	return c, func() { c.cancel(true, Canceled) } //返回 cancel函数（还可以通过cancel函数来cancel上下文）
}

// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
//context.timerCtx 结构体内部通过嵌入了context.cancelCtx 结构体继承了相关的变量和方法，还通过持有的定时器 timer 和截止时间 deadline 实现了定时取消这一功能
type timerCtx struct {
	cancelCtx             // 本次调用创建的 cancelCtx（所有 取消操作都是通过 cancelCtx来实现的）
	timer     *time.Timer // Under cancelCtx.mu. //定时器，当时间超过了截止日期后会调用 context.timerCtx.cancel 方法触发取消信号

	deadline time.Time //截止时间
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

//context.timerCtx.cancel 方法不仅调用了 context.cancelCtx.cancel，还会停止持有的定时器减少不必要的资源浪费。
func (c *timerCtx) cancel(removeFromParent bool, err error) {
	c.cancelCtx.cancel(false, err)
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete:
//
// 	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
// 		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
// 		defer cancel()  // releases resources if slowOperation completes before timeout elapses
// 		return slowOperation(ctx)
// 	}
//内部调用了 WithDeadline，只不过 截止时间是  当前时间+timeout
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithValue returns a copy of parent in which the value associated with key is
// val.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The provided key must be comparable and should not be of type
// string or any other built-in type to avoid collisions between
// packages using context. Users of WithValue should define their own
// types for keys. To avoid allocating when assigning to an
// interface{}, context keys often have concrete type
// struct{}. Alternatively, exported context key variables' static
// type should be a pointer or interface.
//从父上下文中创建一个子上下文，创建的子上下文属于 context.valueCtx 类型
func WithValue(parent Context, key, val interface{}) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() { //如果类型不可比较的话（如果不可比较，那就不能判断当前遍历到的key 是否和 key相等 ）
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
//将除了 Value、String 之外的方法代理到父上下文中，在Value方法中会判断当前的key和 valueCtx中存储的key是否相等，
//如果相等直接返回 存储的value；否则，调用父上下文的Value方法(从父上下文中查找该键对应的值直到在某个父上下文中返回 nil 或者查找到对应的值)
//注意：在真正使用传值的功能时我们应该非常谨慎：context.Value 设计非常差，存储和查询的效率非常低，每存储一个kv对 就创建一个对象，查找的时候 层层查找，非常垃圾。
type valueCtx struct {
	Context              //父上下文
	key, val interface{} //本次存储的kv对
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v interface{}) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	}
	return "<not Stringer>"
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(type " +
		reflectlite.TypeOf(c.key).String() +
		", val " + stringify(c.val) + ")"
}

func (c *valueCtx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	return c.Context.Value(key)
}
