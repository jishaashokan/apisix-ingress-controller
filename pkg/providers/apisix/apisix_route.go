// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package apisix

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	apisixcache "github.com/apache/apisix-ingress-controller/pkg/apisix/cache"
	"github.com/apache/apisix-ingress-controller/pkg/config"
	"github.com/apache/apisix-ingress-controller/pkg/kube"
	v2 "github.com/apache/apisix-ingress-controller/pkg/kube/apisix/apis/config/v2"
	"github.com/apache/apisix-ingress-controller/pkg/kube/apisix/apis/config/v2beta3"
	"github.com/apache/apisix-ingress-controller/pkg/log"
	"github.com/apache/apisix-ingress-controller/pkg/providers/translation"
	"github.com/apache/apisix-ingress-controller/pkg/providers/utils"
	"github.com/apache/apisix-ingress-controller/pkg/types"
	apisixv1 "github.com/apache/apisix-ingress-controller/pkg/types/apisix/v1"
)

type apisixRouteController struct {
	*apisixCommon
	workqueue        workqueue.RateLimitingInterface
	relatedWorkqueue workqueue.RateLimitingInterface
	workers          int

	svcLock sync.RWMutex
	// service key -> apisix route key
	svcMap map[string]map[string]struct{}

	apisixUpstreamLock sync.RWMutex
	// apisix upstream key -> apisix route key
	apisixUpstreamMap map[string]map[string]struct{}
}

type routeEvent struct {
	Key  string
	Type string
}

func newApisixRouteController(common *apisixCommon) *apisixRouteController {
	c := &apisixRouteController{
		apisixCommon:     common,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.NewItemFastSlowRateLimiter(1*time.Second, 60*time.Second, 5), "ApisixRoute"),
		relatedWorkqueue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemFastSlowRateLimiter(1*time.Second, 60*time.Second, 5), "ApisixRouteRelated"),
		workers:          1,

		svcMap:            make(map[string]map[string]struct{}),
		apisixUpstreamMap: make(map[string]map[string]struct{}),
	}

	c.ApisixRouteInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onAdd,
			UpdateFunc: c.onUpdate,
			DeleteFunc: c.onDelete,
		},
	)
	c.SvcInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: c.onSvcAdd,
		},
	)
	c.ApisixUpstreamInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onApisixUpstreamAdd,
			UpdateFunc: c.onApisixUpstreamUpdate,
		},
	)

	return c
}

func (c *apisixRouteController) run(ctx context.Context) {
	log.Info("ApisixRoute controller started")
	defer log.Info("ApisixRoute controller exited")
	defer c.workqueue.ShutDown()
	defer c.relatedWorkqueue.ShutDown()

	for i := 0; i < c.workers; i++ {
		go c.runWorker(ctx)
		go c.runRelatedWorker(ctx)
	}
	<-ctx.Done()
}

func (c *apisixRouteController) runWorker(ctx context.Context) {
	for {
		obj, quit := c.workqueue.Get()
		if quit {
			return
		}

		switch val := obj.(type) {
		case *types.Event:
			err := c.sync(ctx, val)
			c.workqueue.Done(obj)
			c.handleSyncErr(obj, err)
		}
	}
}

func (c *apisixRouteController) runRelatedWorker(ctx context.Context) {
	for {
		obj, quit := c.relatedWorkqueue.Get()
		if quit {
			return
		}

		ev := obj.(*routeEvent)
		switch ev.Type {
		case "service":
			err := c.handleSvcAdd(ev.Key)
			c.workqueue.Done(obj)
			c.handleSvcErr(ev, err)
		case "ApisixUpstream":
			err := c.handleApisixUpstreamChange(ev.Key)
			c.workqueue.Done(obj)
			c.handleApisixUpstreamErr(ev, err)
		}
	}
}

func (c *apisixRouteController) syncRelationship(ev *types.Event, routeKey string, ar kube.ApisixRoute) {
	obj := ev.Object.(kube.ApisixRouteEvent)

	var (
		oldBackends []string
		newBackends []string

		oldUpstreams []string
		newUpstreams []string
	)
	switch obj.GroupVersion {
	case config.ApisixV2beta3:
		var (
			old    *v2beta3.ApisixRoute
			newObj *v2beta3.ApisixRoute
		)

		if ev.Type == types.EventUpdate {
			old = obj.OldObject.V2beta3()
		} else if ev.Type == types.EventDelete {
			old = ev.Tombstone.(kube.ApisixRoute).V2beta3()
		}

		if ev.Type != types.EventDelete {
			newObj = ar.V2beta3()
		}

		// calculate diff, so we don't need to care about the event order
		if old != nil {
			for _, rule := range old.Spec.HTTP {
				for _, backend := range rule.Backends {
					oldBackends = append(oldBackends, old.Namespace+"/"+backend.ServiceName)
				}
			}
		}
		if newObj != nil {
			for _, rule := range newObj.Spec.HTTP {
				for _, backend := range rule.Backends {
					newBackends = append(newBackends, newObj.Namespace+"/"+backend.ServiceName)
				}
			}
		}
	case config.ApisixV2:
		var (
			old    *v2.ApisixRoute
			newObj *v2.ApisixRoute
		)

		if ev.Type == types.EventUpdate {
			old = obj.OldObject.V2()
		} else if ev.Type == types.EventDelete {
			old = ev.Tombstone.(kube.ApisixRoute).V2()
		}

		if ev.Type != types.EventDelete {
			newObj = ar.V2()
		}

		// calculate diff, so we don't need to care about the event order
		if old != nil {
			for _, rule := range old.Spec.HTTP {
				for _, backend := range rule.Backends {
					oldBackends = append(oldBackends, old.Namespace+"/"+backend.ServiceName)
				}

				for _, upstream := range rule.Upstreams {
					oldUpstreams = append(oldUpstreams, old.Namespace+"/"+upstream.Name)
				}
			}
		}
		if newObj != nil {
			for _, rule := range newObj.Spec.HTTP {
				for _, backend := range rule.Backends {
					newBackends = append(newBackends, newObj.Namespace+"/"+backend.ServiceName)
				}
				for _, upstream := range rule.Upstreams {
					newUpstreams = append(newUpstreams, newObj.Namespace+"/"+upstream.Name)
				}
			}
		}
	default:
		log.Errorw("unknown ApisixRoute version",
			zap.String("version", obj.GroupVersion),
			zap.String("key", obj.Key),
		)
	}

	// NOTE:
	// This implementation MAY cause potential problem due to unstable event order
	// The last event processed MAY not be the logical last event, so it may override the logical previous event
	// We have a periodic full-sync, which reduce this problem, but it doesn't solve it completely.

	toDelete := utils.Difference(oldBackends, newBackends)
	toAdd := utils.Difference(newBackends, oldBackends)
	c.syncServiceRelationChanges(routeKey, toAdd, toDelete)

	toDelete = utils.Difference(oldUpstreams, newUpstreams)
	toAdd = utils.Difference(newUpstreams, oldUpstreams)
	c.syncApisixUpstreamRelationChanges(routeKey, toAdd, toDelete)
}

func (c *apisixRouteController) syncServiceRelationChanges(routeKey string, toAdd, toDelete []string) {
	c.svcLock.Lock()
	defer c.svcLock.Unlock()

	for _, svc := range toDelete {
		delete(c.svcMap[svc], routeKey)
	}

	for _, svc := range toAdd {
		if _, ok := c.svcMap[svc]; !ok {
			c.svcMap[svc] = make(map[string]struct{})
		}
		c.svcMap[svc][routeKey] = struct{}{}
	}
}

func (c *apisixRouteController) syncApisixUpstreamRelationChanges(routeKey string, toAdd, toDelete []string) {
	c.apisixUpstreamLock.Lock()
	defer c.apisixUpstreamLock.Unlock()

	for _, au := range toDelete {
		delete(c.apisixUpstreamMap[au], routeKey)
	}

	for _, au := range toAdd {
		if _, ok := c.apisixUpstreamMap[au]; !ok {
			c.apisixUpstreamMap[au] = make(map[string]struct{})
		}
		c.apisixUpstreamMap[au][routeKey] = struct{}{}
	}
}

func (c *apisixRouteController) sync(ctx context.Context, ev *types.Event) error {
	obj := ev.Object.(kube.ApisixRouteEvent)
	namespace, name, err := cache.SplitMetaNamespaceKey(obj.Key)
	if err != nil {
		log.Errorf("invalid resource key: %s", obj.Key)
		return err
	}
	var (
		ar   kube.ApisixRoute
		tctx *translation.TranslateContext
	)
	switch obj.GroupVersion {
	case config.ApisixV2beta3:
		ar, err = c.ApisixRouteLister.V2beta3(namespace, name)
	case config.ApisixV2:
		ar, err = c.ApisixRouteLister.V2(namespace, name)
	default:
		log.Errorw("unknown ApisixRoute version",
			zap.String("version", obj.GroupVersion),
			zap.String("key", obj.Key),
		)
		return fmt.Errorf("unknown ApisixRoute version %v", obj.GroupVersion)
	}
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Errorw("failed to get ApisixRoute",
				zap.String("version", obj.GroupVersion),
				zap.String("key", obj.Key),
				zap.Error(err),
			)
			return err
		}

		if ev.Type != types.EventDelete {
			log.Warnw("ApisixRoute was deleted before it can be delivered",
				zap.String("key", obj.Key),
				zap.String("version", obj.GroupVersion),
			)
			return nil
		}
	}

	// sync before translation
	c.syncRelationship(ev, obj.Key, ar)

	if ev.Type == types.EventDelete {
		if ar != nil {
			// We still find the resource while we are processing the DELETE event,
			// that means object with same namespace and name was created, discarding
			// this stale DELETE event.
			log.Warnw("discard the stale ApisixRoute delete event since the resource still exists",
				zap.String("key", obj.Key),
			)
			return nil
		}
		ar = ev.Tombstone.(kube.ApisixRoute)
	}

	switch obj.GroupVersion {
	case config.ApisixV2beta3:
		if ev.Type != types.EventDelete {
			if err = c.checkPluginNameIfNotEmptyV2beta3(ctx, ar.V2beta3()); err == nil {
				tctx, err = c.translator.TranslateRouteV2beta3(ar.V2beta3())
			}
		} else {
			tctx, err = c.translator.GenerateRouteV2beta3DeleteMark(ar.V2beta3())
		}
		if err != nil {
			log.Errorw("failed to translate ApisixRoute v2beta3",
				zap.Error(err),
				zap.Any("object", ar),
			)
			return err
		}
	case config.ApisixV2:
		if ev.Type != types.EventDelete {
			if err = c.checkPluginNameIfNotEmptyV2(ctx, ar.V2()); err == nil {
				tctx, err = c.translator.TranslateRouteV2(ar.V2())
			}
		} else {
			tctx, err = c.translator.GenerateRouteV2DeleteMark(ar.V2())
		}
		if err != nil {
			log.Errorw("failed to translate ApisixRoute v2",
				zap.Error(err),
				zap.Any("object", ar),
			)
			return err
		}
	default:
		log.Errorw("unknown ApisixRoute version",
			zap.String("version", obj.GroupVersion),
			zap.String("key", obj.Key),
		)
		return fmt.Errorf("unknown ApisixRoute version %v", obj.GroupVersion)
	}

	log.Debugw("translated ApisixRoute",
		zap.Any("routes", tctx.Routes),
		zap.Any("upstreams", tctx.Upstreams),
		zap.Any("apisix_route", ar),
		zap.Any("pluginConfigs", tctx.PluginConfigs),
	)

	m := &utils.Manifest{
		Routes:        tctx.Routes,
		Upstreams:     tctx.Upstreams,
		StreamRoutes:  tctx.StreamRoutes,
		PluginConfigs: tctx.PluginConfigs,
	}

	var (
		added   *utils.Manifest
		updated *utils.Manifest
		deleted *utils.Manifest
	)

	if ev.Type == types.EventDelete {
		deleted = m
	} else if ev.Type == types.EventAdd {
		added = m
	} else {
		oldCtx, _ := c.translator.TranslateOldRoute(obj.OldObject)
		om := &utils.Manifest{
			Routes:        oldCtx.Routes,
			Upstreams:     oldCtx.Upstreams,
			StreamRoutes:  oldCtx.StreamRoutes,
			PluginConfigs: oldCtx.PluginConfigs,
		}
		added, updated, deleted = m.Diff(om)
	}

	return c.SyncManifests(ctx, added, updated, deleted)
}

func (c *apisixRouteController) checkPluginNameIfNotEmptyV2beta3(ctx context.Context, in *v2beta3.ApisixRoute) error {
	for _, v := range in.Spec.HTTP {
		if v.PluginConfigName != "" {
			_, err := c.APISIX.Cluster(c.Config.APISIX.DefaultClusterName).PluginConfig().Get(ctx, apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName))
			if err != nil {
				if err == apisixcache.ErrNotFound {
					log.Errorw("checkPluginNameIfNotEmptyV2beta3 error: plugin_config not found",
						zap.String("name", apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName)),
						zap.Any("obj", in),
						zap.Error(err))
				} else {
					log.Errorw("checkPluginNameIfNotEmptyV2beta3 PluginConfig get failed",
						zap.String("name", apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName)),
						zap.Any("obj", in),
						zap.Error(err))
				}
				return err
			}
		}
	}
	return nil
}

func (c *apisixRouteController) checkPluginNameIfNotEmptyV2(ctx context.Context, in *v2.ApisixRoute) error {
	for _, v := range in.Spec.HTTP {
		if v.PluginConfigName != "" {
			_, err := c.APISIX.Cluster(c.Config.APISIX.DefaultClusterName).PluginConfig().Get(ctx, apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName))
			if err != nil {
				if err == apisixcache.ErrNotFound {
					log.Errorw("checkPluginNameIfNotEmptyV2 error: plugin_config not found",
						zap.String("name", apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName)),
						zap.Any("obj", in),
						zap.Error(err))
				} else {
					log.Errorw("checkPluginNameIfNotEmptyV2 PluginConfig get failed",
						zap.String("name", apisixv1.ComposePluginConfigName(in.Namespace, v.PluginConfigName)),
						zap.Any("obj", in),
						zap.Error(err))
				}
				return err
			}
		}
	}
	return nil
}

func (c *apisixRouteController) handleSyncErr(obj interface{}, errOrigin error) {
	ev := obj.(*types.Event)
	event := ev.Object.(kube.ApisixRouteEvent)
	if k8serrors.IsNotFound(errOrigin) && ev.Type != types.EventDelete {
		log.Infow("sync ApisixRoute but not found, ignore",
			zap.String("event_type", ev.Type.String()),
			zap.String("ApisixRoute", event.Key),
		)
		c.workqueue.Forget(event)
		return
	}
	namespace, name, errLocal := cache.SplitMetaNamespaceKey(event.Key)
	if errLocal != nil {
		log.Errorf("invalid resource key: %s", event.Key)
		c.MetricsCollector.IncrSyncOperation("route", "failure")
		return
	}
	var ar kube.ApisixRoute
	switch event.GroupVersion {
	case config.ApisixV2beta3:
		ar, errLocal = c.ApisixRouteLister.V2beta3(namespace, name)
	case config.ApisixV2:
		ar, errLocal = c.ApisixRouteLister.V2(namespace, name)
	default:
		log.Errorw("unknown ApisixRoute version",
			zap.String("version", event.GroupVersion),
			zap.String("key", event.Key),
		)
	}
	if errOrigin == nil {
		if ev.Type != types.EventDelete {
			if errLocal == nil {
				switch ar.GroupVersion() {
				case config.ApisixV2beta3:
					c.RecordEvent(ar.V2beta3(), v1.EventTypeNormal, utils.ResourceSynced, nil)
					c.recordStatus(ar.V2beta3(), utils.ResourceSynced, nil, metav1.ConditionTrue, ar.V2beta3().GetGeneration())
				case config.ApisixV2:
					c.RecordEvent(ar.V2(), v1.EventTypeNormal, utils.ResourceSynced, nil)
					c.recordStatus(ar.V2(), utils.ResourceSynced, nil, metav1.ConditionTrue, ar.V2().GetGeneration())
				}
			} else {
				log.Errorw("failed list ApisixRoute",
					zap.Error(errLocal),
					zap.String("name", name),
					zap.String("namespace", namespace),
				)
			}
		}
		c.workqueue.Forget(obj)
		c.MetricsCollector.IncrSyncOperation("route", "success")
		return
	}
	log.Warnw("sync ApisixRoute failed, will retry",
		zap.Any("object", obj),
		zap.Error(errOrigin),
	)
	if errLocal == nil {
		switch ar.GroupVersion() {
		case config.ApisixV2beta3:
			c.RecordEvent(ar.V2beta3(), v1.EventTypeWarning, utils.ResourceSyncAborted, errOrigin)
			c.recordStatus(ar.V2beta3(), utils.ResourceSyncAborted, errOrigin, metav1.ConditionFalse, ar.V2beta3().GetGeneration())
		case config.ApisixV2:
			c.RecordEvent(ar.V2(), v1.EventTypeWarning, utils.ResourceSyncAborted, errOrigin)
			c.recordStatus(ar.V2(), utils.ResourceSyncAborted, errOrigin, metav1.ConditionFalse, ar.V2().GetGeneration())
		}
	} else {
		log.Errorw("failed list ApisixRoute",
			zap.Error(errLocal),
			zap.String("name", name),
			zap.String("namespace", namespace),
		)
	}
	c.workqueue.AddRateLimited(obj)
	c.MetricsCollector.IncrSyncOperation("route", "failure")
}

func (c *apisixRouteController) onAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorf("found ApisixRoute resource with bad meta namespace key: %s", err)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}
	log.Debugw("ApisixRoute add event arrived",
		zap.String("key", key),
		zap.Any("object", obj),
	)

	ar := kube.MustNewApisixRoute(obj)
	c.workqueue.Add(&types.Event{
		Type: types.EventAdd,
		Object: kube.ApisixRouteEvent{
			Key:          key,
			GroupVersion: ar.GroupVersion(),
		},
	})

	c.MetricsCollector.IncrEvents("route", "add")
}

func (c *apisixRouteController) onUpdate(oldObj, newObj interface{}) {
	prev := kube.MustNewApisixRoute(oldObj)
	curr := kube.MustNewApisixRoute(newObj)
	if prev.ResourceVersion() >= curr.ResourceVersion() {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		log.Errorf("found ApisixRoute resource with bad meta namespace key: %s", err)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}
	log.Debugw("ApisixRoute update event arrived",
		zap.String("key", key),
		zap.Any("new object", oldObj),
		zap.Any("old object", newObj),
	)
	c.workqueue.Add(&types.Event{
		Type: types.EventUpdate,
		Object: kube.ApisixRouteEvent{
			Key:          key,
			GroupVersion: curr.GroupVersion(),
			OldObject:    prev,
		},
	})

	c.MetricsCollector.IncrEvents("route", "update")
}

func (c *apisixRouteController) onDelete(obj interface{}) {
	ar, err := kube.NewApisixRoute(obj)
	if err != nil {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		ar = kube.MustNewApisixRoute(tombstone)
	}
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorf("found ApisixRoute resource with bad meta namesapce key: %s", err)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}
	log.Debugw("ApisixRoute delete event arrived",
		zap.String("key", key),
		zap.Any("final state", ar),
	)
	c.workqueue.Add(&types.Event{
		Type: types.EventDelete,
		Object: kube.ApisixRouteEvent{
			Key:          key,
			GroupVersion: ar.GroupVersion(),
		},
		Tombstone: ar,
	})

	c.MetricsCollector.IncrEvents("route", "delete")
}

func (c *apisixRouteController) ResourceSync() {
	objs := c.ApisixRouteInformer.GetIndexer().List()

	c.svcLock.Lock()
	c.apisixUpstreamLock.Lock()
	defer c.svcLock.Unlock()
	defer c.apisixUpstreamLock.Unlock()

	c.svcMap = make(map[string]map[string]struct{})
	c.apisixUpstreamMap = make(map[string]map[string]struct{})

	for _, obj := range objs {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			log.Errorw("ApisixRoute sync failed, found ApisixRoute resource with bad meta namespace key",
				zap.Error(err),
			)
			continue
		}
		if !c.namespaceProvider.IsWatchingNamespace(key) {
			continue
		}
		ar := kube.MustNewApisixRoute(obj)
		c.workqueue.Add(&types.Event{
			Type: types.EventAdd,
			Object: kube.ApisixRouteEvent{
				Key:          key,
				GroupVersion: ar.GroupVersion(),
			},
		})

		ns, _, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			log.Errorw("split ApisixRoute meta key failed",
				zap.Error(err),
				zap.String("key", key),
			)
			continue
		}

		var (
			backends  []string
			upstreams []string
		)
		switch ar.GroupVersion() {
		case config.ApisixV2beta3:
			for _, rule := range ar.V2beta3().Spec.HTTP {
				for _, backend := range rule.Backends {
					backends = append(backends, ns+"/"+backend.ServiceName)
				}
			}
		case config.ApisixV2:
			for _, rule := range ar.V2().Spec.HTTP {
				for _, backend := range rule.Backends {
					backends = append(backends, ns+"/"+backend.ServiceName)
				}
				for _, upstream := range rule.Upstreams {
					upstreams = append(upstreams, ns+"/"+upstream.Name)
				}
			}
		default:
			log.Errorw("unknown ApisixRoute version",
				zap.String("version", ar.GroupVersion()),
				zap.String("key", key),
			)
		}
		for _, svcKey := range backends {
			if _, ok := c.svcMap[svcKey]; !ok {
				c.svcMap[svcKey] = make(map[string]struct{})
			}
			c.svcMap[svcKey][key] = struct{}{}
		}
		for _, upstreamKey := range upstreams {
			if _, ok := c.apisixUpstreamMap[upstreamKey]; !ok {
				c.apisixUpstreamMap[upstreamKey] = make(map[string]struct{})
			}
			c.apisixUpstreamMap[upstreamKey][key] = struct{}{}
		}
	}
}

func (c *apisixRouteController) onSvcAdd(obj interface{}) {
	log.Debugw("Service add event arrived",
		zap.Any("object", obj),
	)
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorw("found Service with bad meta key",
			zap.Error(err),
			zap.Any("obj", obj),
		)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}

	c.relatedWorkqueue.Add(&routeEvent{
		Key:  key,
		Type: "service",
	})
}

func (c *apisixRouteController) handleSvcAdd(key string) error {
	log.Debugw("handle svc add", zap.String("key", key))
	c.svcLock.RLock()
	routes, ok := c.svcMap[key]
	c.svcLock.RUnlock()

	if ok {
		for routeKey := range routes {
			c.workqueue.Add(&types.Event{
				Type: types.EventAdd,
				Object: kube.ApisixRouteEvent{
					Key:          routeKey,
					GroupVersion: c.Kubernetes.APIVersion,
				},
			})
		}
	}
	return nil
}

func (c *apisixRouteController) handleSvcErr(ev *routeEvent, errOrigin error) {
	if errOrigin == nil {
		c.workqueue.Forget(ev)

		return
	}

	log.Warnw("sync Service failed, will retry",
		zap.Any("key", ev.Key),
		zap.Error(errOrigin),
	)
	c.relatedWorkqueue.AddRateLimited(ev)
}

func (c *apisixRouteController) onApisixUpstreamAdd(obj interface{}) {
	log.Debugw("ApisixUpstream add event arrived",
		zap.Any("object", obj),
	)
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorw("found Service with bad meta key",
			zap.Error(err),
			zap.Any("obj", obj),
		)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}

	c.relatedWorkqueue.Add(&routeEvent{
		Key:  key,
		Type: "ApisixUpstream",
	})
}

func (c *apisixRouteController) onApisixUpstreamUpdate(oldObj, newObj interface{}) {
	log.Debugw("ApisixUpstream add event arrived",
		zap.Any("object", newObj),
	)

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		log.Errorf("found ApisixUpstream resource with bad meta namespace key: %s", err)
		return
	}
	if err != nil {
		log.Errorw("found Service with bad meta key",
			zap.Error(err),
			zap.Any("obj", newObj),
		)
		return
	}
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}

	c.relatedWorkqueue.Add(&routeEvent{
		Key:  key,
		Type: "ApisixUpstream",
	})
}

func (c *apisixRouteController) handleApisixUpstreamChange(key string) error {
	c.svcLock.RLock()
	routes, ok := c.apisixUpstreamMap[key]
	c.svcLock.RUnlock()

	if ok {
		for routeKey := range routes {
			c.workqueue.Add(&types.Event{
				Type: types.EventAdd,
				Object: kube.ApisixRouteEvent{
					Key:          routeKey,
					GroupVersion: c.Kubernetes.APIVersion,
				},
			})
		}
	}
	return nil
}

func (c *apisixRouteController) handleApisixUpstreamErr(ev *routeEvent, errOrigin error) {
	if errOrigin == nil {
		c.workqueue.Forget(ev)

		return
	}

	log.Warnw("sync ApisixUpstream add event failed, will retry",
		zap.Any("key", ev.Key),
		zap.Error(errOrigin),
	)
	c.workqueue.AddRateLimited(ev)
}

// recordStatus record resources status
func (c *apisixRouteController) recordStatus(at interface{}, reason string, err error, status metav1.ConditionStatus, generation int64) {
	// build condition
	message := utils.CommonSuccessMessage
	if err != nil {
		message = err.Error()
	}
	condition := metav1.Condition{
		Type:               utils.ConditionType,
		Reason:             reason,
		Status:             status,
		Message:            message,
		ObservedGeneration: generation,
	}
	apisixClient := c.KubeClient.APISIXClient

	if kubeObj, ok := at.(runtime.Object); ok {
		at = kubeObj.DeepCopyObject()
	}

	switch v := at.(type) {
	case *v2beta3.ApisixRoute:
		// set to status
		if v.Status.Conditions == nil {
			conditions := make([]metav1.Condition, 0)
			v.Status.Conditions = conditions
		}
		if utils.VerifyGeneration(&v.Status.Conditions, condition) {
			meta.SetStatusCondition(&v.Status.Conditions, condition)
			if _, errRecord := apisixClient.ApisixV2beta3().ApisixRoutes(v.Namespace).
				UpdateStatus(context.TODO(), v, metav1.UpdateOptions{}); errRecord != nil {
				log.Errorw("failed to record status change for ApisixRoute",
					zap.Error(errRecord),
					zap.String("name", v.Name),
					zap.String("namespace", v.Namespace),
				)
			}
		}
	case *v2.ApisixRoute:
		// set to status
		if v.Status.Conditions == nil {
			conditions := make([]metav1.Condition, 0)
			v.Status.Conditions = conditions
		}
		if utils.VerifyGeneration(&v.Status.Conditions, condition) {
			meta.SetStatusCondition(&v.Status.Conditions, condition)
			if _, errRecord := apisixClient.ApisixV2().ApisixRoutes(v.Namespace).
				UpdateStatus(context.TODO(), v, metav1.UpdateOptions{}); errRecord != nil {
				log.Errorw("failed to record status change for ApisixRoute",
					zap.Error(errRecord),
					zap.String("name", v.Name),
					zap.String("namespace", v.Namespace),
				)
			}
		}
	default:
		// This should not be executed
		log.Errorf("unsupported resource record: %s", v)
	}
}

func (c *apisixRouteController) NotifyServiceAdd(key string) {
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}

	c.relatedWorkqueue.Add(&routeEvent{
		Key:  key,
		Type: "service",
	})
}

func (c *apisixRouteController) NotifyApisixUpstreamChange(key string) {
	if !c.namespaceProvider.IsWatchingNamespace(key) {
		return
	}

	c.relatedWorkqueue.Add(&routeEvent{
		Key:  key,
		Type: "ApisixUpstream",
	})
}
