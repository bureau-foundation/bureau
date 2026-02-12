// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package clock

import "time"

// Real returns a Clock backed by the standard time package.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) AfterFunc(d time.Duration, f func()) *Timer {
	timer := time.AfterFunc(d, f)
	return &Timer{
		C:         nil,
		stopFunc:  timer.Stop,
		resetFunc: timer.Reset,
	}
}

func (realClock) NewTicker(d time.Duration) *Ticker {
	ticker := time.NewTicker(d)
	return &Ticker{
		C:         ticker.C,
		stopFunc:  ticker.Stop,
		resetFunc: ticker.Reset,
	}
}

func (realClock) Sleep(d time.Duration) { time.Sleep(d) }
