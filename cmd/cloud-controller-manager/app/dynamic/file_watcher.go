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

	"sigs.k8s.io/cloud-provider-azure/pkg/log"
)

func RunFileWatcherOrDie(path string) chan struct{} {
	logger := log.Background().WithName("RunFileWatcherOrDie")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Error(err, "RunFileWatcherOrDie: failed to initialize file watcher")
		os.Exit(1)
	}

	startWatchingOrDie(watcher, path, 5)

	updateChan := make(chan struct{})
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					logger.Error(nil, "RunFileWatcherOrDie: events channel closed unexpectedly")
					_ = watcher.Close()
					os.Exit(1)
				}

				logger.Info("found file update event", "event", event)
				updateChan <- struct{}{}

				if strings.EqualFold(event.Name, path) && event.Op&fsnotify.Remove == fsnotify.Remove {
					startWatchingOrDie(watcher, path, 5)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					logger.Error(nil, "RunFileWatcherOrDie: errors channel closed unexpectedly")
					_ = watcher.Close()
					os.Exit(1)
				}

				logger.Error(err, "RunFileWatcherOrDie: failed to watch file", "path", path)
				_ = watcher.Close()
				os.Exit(1)
			}
		}
	}()

	return updateChan
}

func startWatchingOrDie(watcher *fsnotify.Watcher, path string, maxRetries int) {
	logger := log.Background().WithName("startWatchingOrDie")
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
			logger.Error(err, "RunFileWatcherOrDie: failed to watch after retries", "path", path, "maxRetries", maxRetries)
			_ = watcher.Close()
			os.Exit(1)
		}

		break
	}
}
