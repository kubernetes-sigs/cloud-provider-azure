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
	"fmt"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	cloudcontrollerconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
	"sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/options"
)

type SecretWatcher struct {
	informerFactory informers.SharedInformerFactory
	secretInformer  coreinformers.SecretInformer
}

// Run starts shared informers and waits for the shared informer cache to
// synchronize.
func (c *SecretWatcher) Run(stopCh <-chan struct{}) error {
	c.informerFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.secretInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to operation the initial list of secrets")
	}

	return nil
}

func RunSecretWatcherOrDie(c *cloudcontrollerconfig.Config) chan struct{} {
	factory := informers.NewSharedInformerFactory(c.VersionedClient, options.ResyncPeriod(c)())
	secretWatcher, updateCh := NewSecretWatcher(factory, c.DynamicReloadingConfig.CloudConfigSecretName, c.DynamicReloadingConfig.CloudConfigSecretNamespace)
	err := secretWatcher.Run(wait.NeverStop)
	if err != nil {
		klog.Errorf("Run: failed to initialize secret watcher: %v", err)
		os.Exit(1)
	}

	return updateCh
}

// NewSecretWatcher creates a SecretWatcher and a signal channel to indicate
// the specific secret has been updated
func NewSecretWatcher(informerFactory informers.SharedInformerFactory, secretName, secretNamespace string) (*SecretWatcher, chan struct{}) {
	secretInformer := informerFactory.Core().V1().Secrets()
	updateSignal := make(chan struct{})

	secretWatcher := &SecretWatcher{
		informerFactory: informerFactory,
		secretInformer:  secretInformer,
	}
	_, _ = secretInformer.Informer().AddEventHandler(
		// Your custom resource event handlers.
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, newObj interface{}) {
				newSecret := newObj.(*v1.Secret)

				if strings.EqualFold(newSecret.Name, secretName) &&
					strings.EqualFold(newSecret.Namespace, secretNamespace) {
					klog.V(1).Infof("secret %s updated, sending the signal", newSecret.Name)
					updateSignal <- struct{}{}
				}
			},
		},
	)

	return secretWatcher, updateSignal
}
