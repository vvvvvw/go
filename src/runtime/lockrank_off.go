// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !goexperiment.staticlockranking

package runtime

// // lockRankStruct is embedded in mutex, but is empty when staticklockranking is
// disabled (the default)
//lockRankStruct嵌入在互斥锁mutex结构体中，但当staticklockranking（默认就是被禁用）被禁用时，本结构体为空
type lockRankStruct struct {
}

func lockInit(l *mutex, rank lockRank) {
}

func getLockRank(l *mutex) lockRank {
	return 0
}

func lockWithRank(l *mutex, rank lockRank) {
	lock2(l)
}

func acquireLockRank(rank lockRank) {
}

func unlockWithRank(l *mutex) {
	unlock2(l)
}

func releaseLockRank(rank lockRank) {
}

func lockWithRankMayAcquire(l *mutex, rank lockRank) {
}
