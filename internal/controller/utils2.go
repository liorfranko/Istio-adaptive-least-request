/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sync"
	"sync/atomic"
)

type tKeyToMuxKey struct {
	Namespace string
	Name      string
}

type tMarkMux struct {
	mux     sync.Mutex
	touched int64
}

func (m *tMarkMux) checkTouchedAndReset() bool {
	return atomic.SwapInt64(&m.touched, 0) > 0
}

var (
	keyToMuxMux sync.Mutex
	keyToMux    = make(map[tKeyToMuxKey]*tMarkMux)
)

func getMux(namespace, name string) *tMarkMux {
	key := tKeyToMuxKey{
		Namespace: namespace,
		Name:      name,
	}
	keyToMuxMux.Lock()
	defer keyToMuxMux.Unlock()
	markMux, ok := keyToMux[key]
	if !ok {
		markMux = new(tMarkMux)
		keyToMux[key] = markMux
	}
	return markMux
}

func tryLock(namespace, name string) (bool, func(), func() bool) {
	markMux := getMux(namespace, name)
	if !markMux.mux.TryLock() {
		atomic.AddInt64(&markMux.touched, 1)
		return false, nil, nil
	}
	return true, markMux.mux.Unlock, markMux.checkTouchedAndReset
}
