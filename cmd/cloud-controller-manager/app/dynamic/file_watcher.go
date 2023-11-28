/*
Copyright 2021 The Kubernetes Authors.

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

package dynamic

import (
	"os"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"k8s.io/klog/v2"
)

func RunFileWatcherOrDie(path string) chan struct{} {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Errorf("RunFileWatcherOrDie: failed to initialize file watcher: %s", err)
		os.Exit(1)
	}

	startWatchingOrDie(watcher, path, 5)

	updateChan := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					klog.Error("RunFileWatcherOrDie: events channel closed unexpectedly")
					_ = watcher.Close()
					os.Exit(1)
				}

				klog.Infof("RunFileWatcherOrDie: found file update event: %v", event)
				updateChan <- struct{}{}

				if strings.EqualFold(event.Name, path) && event.Op&fsnotify.Remove == fsnotify.Remove {
					startWatchingOrDie(watcher, path, 5)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					klog.Error("RunFileWatcherOrDie: errors channel closed unexpectedly")
					_ = watcher.Close()
					os.Exit(1)
				}

				klog.Errorf("RunFileWatcherOrDie: failed to watch file %s: %s", path, err.Error())
				_ = watcher.Close()
				os.Exit(1)
			}
		}
	}()

	return updateChan
}

func startWatchingOrDie(watcher *fsnotify.Watcher, path string, maxRetries int) {
	attempt := 0
	for {
		err := watcher.Add(path)
		if err != nil {
			if attempt < maxRetries {
				attempt++
				klog.Warningf("RunFileWatcherOrDie: failed to watch %s: %s, will retry", path, err.Error())
				time.Sleep(time.Second)
				continue
			}
			klog.Errorf("RunFileWatcherOrDie: failed to watch %s after %d times retry", path, maxRetries)
			_ = watcher.Close()
			os.Exit(1)
		}

		break
	}
}
