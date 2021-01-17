// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32  //表示当前互斥锁的状态  最低三位分别表示 mutexLocked（表示互斥锁的锁定状态）、mutexWoken（表示从正常模式被唤醒） 和 mutexStarving（当前的互斥锁进入饥饿状态），剩下的位置用来表示当前有多少个 Goroutine 等待互斥锁的释放
	sema  uint32 //信号量, 如果为0 表示 信号 用完了
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked //1：表示互斥锁已经被锁定
	mutexWoken                   //2：表示当前有goroutine在自旋，设置此标志，当mutex被解锁时不要唤醒阻塞goroutine，这样才能把 mutex尽可能给 正在自旋的线程（节省了 线程状态切换的开销）
	mutexStarving                //4：当前的互斥锁进入饥饿状态
	mutexWaiterShift = iota      //3

	// Mutex fairn ess.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) { //如果未锁定，则cas加锁（将 mutexLocked 位置成 1），如果成功，直接返回
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined) slow path（不是内联的，fast path是内联的）
	m.lockSlow() // 如果互斥锁的状态不是 0 时就会调用 sync.Mutex.lockSlow 尝试通过自旋（Spinnig）等方式等待锁的释放
}

//通过自旋（Spinnig）等方式等待锁的释放
func (m *Mutex) lockSlow() {
	//判断当前 Goroutine 能否进入自旋（自旋会消耗cpu，因此能进入自旋的条件比较苛刻：
	//1.互斥锁只有在普通模式才能进入自旋；
	//2.sync.runtime_canSpin 需要返回 true：
	//3.运行在多 CPU 的机器上；
	//4.当前 Goroutine 为了获取该锁进入自旋的次数小于四次；
	//5.当前机器上至少存在一个正在运行的处理器 P 并且处理的运行队列为空；）
	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		if old&(mutexLocked|mutexStarving) == mutexLocked /*当前互斥锁处于普通模式*/ && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			//主动 自旋很有意义。 如果当前goroutine处于自旋状态，尝试设置MutexWoken标志通知 解锁 者
			//不要唤醒其他被阻塞的goroutine。（这样就只有 自旋线程能获取锁了）
			if !awoke && old&mutexWoken == 0 /*当前没有处于 mutexWoken状态*/ && old>>mutexWaiterShift != 0 /*当前有其他阻塞在本mutex上的进程*/ &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) { //设置 mutexWoken标记
				awoke = true
			}
			//调用sync.runtime_doSpin，内部调用了runtime.procyield，runtime.procyield内部执行了 30 次的 PAUSE 指令，该指令只会占用 CPU 并消耗 CPU 时间
			runtime_doSpin()
			iter++
			//获取当前互斥锁的最新状态，重新开始循环
			old = m.state
			continue
		}
		//如果 不满足 spinning的条件
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 { //不在饥饿模式的时候才尝试获取锁
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 { //如果当前是处于 饥饿模式 或者 当前是处于被锁状态，那么将等待，因此将等待者数量+1
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		if starving && old&mutexLocked != 0 { //如果当前goroutine发现超过 1ms 没有获取到锁，且此时 处于被锁状态，则切换到 饥饿模式
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			//如果当前goroutine在自旋的时候 设置过 mutexWoken标记，那么现在不自旋了，关掉
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) { //如果cas成功
			if old&(mutexLocked|mutexStarving) == 0 { //如果 之前不是处于 被锁状态 或者 饥饿状态，则能加锁，此时cas成功 表示加锁成功，返回
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0 //如果 waitStartTime不为0，表示之前就已经过了自旋执行到这一步了，那么此时 queueLifo设置为true
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime() //将waitStartTime设置为 当前时间
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1) //调用 sync.runtime_SemacquireMutex 使用信号量保证资源不会被两个 Goroutine 获取
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state //更新old字段，重新循环
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1)
	}
}
