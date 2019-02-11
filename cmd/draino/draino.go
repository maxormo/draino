/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/oklog/run"
	"go.opencensus.io/exporter/prometheus"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	"gopkg.in/alecthomas/kingpin.v2"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/planetlabs/draino/internal/kubernetes"
)

// TODO(negz): Use leader election? We don't really want more than one draino
// running at a time.
// https://godoc.org/k8s.io/client-go/tools/leaderelection
func main() {
	var (
		app = kingpin.New(filepath.Base(os.Args[0]), "Automatically cordons and drains nodes that match the supplied conditions.").DefaultEnvars()

		debug            = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		listen           = app.Flag("listen", "Address at which to expose /metrics and /healthz.").Default(":10002").String()
		kubecfg          = app.Flag("kubeconfig", "Path to kubeconfig file. Leave unset to use in-cluster config.").String()
		apiserver        = app.Flag("master", "Address of Kubernetes API server. Leave unset to use in-cluster config.").String()
		dryRun           = app.Flag("dry-run", "Emit an event without cordoning or draining matching nodes.").Bool()
		maxGracePeriod   = app.Flag("max-grace-period", "Maximum time evicted pods will be given to terminate gracefully.").Default(kubernetes.DefaultMaxGracePeriod.String()).Duration()
		evictionHeadroom = app.Flag("eviction-headroom", "Additional time to wait after a pod's termination grace period for it to have been deleted.").Default(kubernetes.DefaultEvictionOverhead.String()).Duration()
		drainBuffer      = app.Flag("drain-buffer", "Minimum time between starting each drain. Nodes are always cordoned immediately.").Default(kubernetes.DefaultDrainBuffer.String()).Duration()
		nodeLabels       = app.Flag("node-label", "Only nodes with this label will be eligible for cordoning and draining. May be specified multiple times.").PlaceHolder("KEY=VALUE").StringMap()

		evictDaemonSetPods    = app.Flag("evict-daemonset-pods", "Evict pods that were created by an extant DaemonSet.").Bool()
		evictLocalStoragePods = app.Flag("evict-emptydir-pods", "Evict pods with local storage, i.e. with emptyDir volumes.").Bool()
		evictUnreplicatedPods = app.Flag("evict-unreplicated-pods", "Evict pods that were not created by a replication controller.").Bool()

		protectedPodAnnotations = app.Flag("protected-pod-annotation", "Protect pods with this annotation from eviction. May be specified multiple times.").PlaceHolder("KEY[=VALUE]").Strings()

		conditions = app.Arg("node-conditions", "Nodes for which any of these conditions are true will be cordoned and drained. <TYPE[=STATE]>").Strings()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	glogWorkaround()

	var (
		nodesCordoned = &view.View{
			Name:        "cordoned_nodes_total",
			Measure:     kubernetes.MeasureNodesCordoned,
			Description: "Number of nodes cordoned.",
			Aggregation: view.Count(),
			TagKeys:     []tag.Key{kubernetes.TagResult},
		}
		nodesDrained = &view.View{
			Name:        "drained_nodes_total",
			Measure:     kubernetes.MeasureNodesDrained,
			Description: "Number of nodes drained.",
			Aggregation: view.Count(),
			TagKeys:     []tag.Key{kubernetes.TagResult},
		}
	)
	kingpin.FatalIfError(view.Register(nodesCordoned, nodesDrained), "cannot create metrics")
	p, err := prometheus.NewExporter(prometheus.Options{Namespace: kubernetes.Component})
	kingpin.FatalIfError(err, "cannot export metrics")
	view.RegisterExporter(p)

	web := &httpRunner{l: *listen, h: map[string]http.Handler{
		"/metrics": p,
		"/healthz": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Body.Close() }), // nolint:gosec
	}}

	log, err := zap.NewProduction()
	if *debug {
		log, err = zap.NewDevelopment()
	}
	kingpin.FatalIfError(err, "cannot create log")
	defer log.Sync()

	c, err := kubernetes.BuildConfigFromFlags(*apiserver, *kubecfg)
	kingpin.FatalIfError(err, "cannot create Kubernetes client configuration")

	cs, err := client.NewForConfig(c)
	kingpin.FatalIfError(err, "cannot create Kubernetes client")

	pf := []kubernetes.PodFilterFunc{kubernetes.MirrorPodFilter}
	if !*evictLocalStoragePods {
		pf = append(pf, kubernetes.LocalStoragePodFilter)
	}
	if !*evictUnreplicatedPods {
		pf = append(pf, kubernetes.UnreplicatedPodFilter)
	}
	if !*evictDaemonSetPods {
		pf = append(pf, kubernetes.NewDaemonSetPodFilter(cs))
	}
	if len(*protectedPodAnnotations) > 0 {
		pf = append(pf, kubernetes.UnprotectedPodFilter(*protectedPodAnnotations...))
	}
	var h cache.ResourceEventHandler = kubernetes.NewDrainingResourceEventHandler(
		kubernetes.NewAPICordonDrainer(cs,
			kubernetes.MaxGracePeriod(*maxGracePeriod),
			kubernetes.EvictionHeadroom(*evictionHeadroom),
			kubernetes.WithPodFilter(kubernetes.NewPodFilters(pf...))),
		kubernetes.NewEventRecorder(cs),
		kubernetes.WithLogger(log),
		kubernetes.WithDrainBuffer(*drainBuffer))

	if *dryRun {
		h = cache.FilteringResourceEventHandler{
			FilterFunc: kubernetes.NewNodeProcessed().Filter,
			Handler: kubernetes.NewDrainingResourceEventHandler(
				&kubernetes.NoopCordonDrainer{},
				kubernetes.NewEventRecorder(cs),
				kubernetes.WithLogger(log),
				kubernetes.WithDrainBuffer(*drainBuffer)),
		}
	}

	sf := cache.FilteringResourceEventHandler{FilterFunc: kubernetes.NodeSchedulableFilter, Handler: h}
	cf := cache.FilteringResourceEventHandler{FilterFunc: kubernetes.NewNodeConditionFilter(*conditions), Handler: sf}
	lf := cache.FilteringResourceEventHandler{FilterFunc: kubernetes.NewNodeLabelFilter(*nodeLabels), Handler: cf}
	nodes := kubernetes.NewNodeWatch(cs, lf)

	kingpin.FatalIfError(await(nodes, web), "error serving")
}

type runner interface {
	Run(stop <-chan struct{})
}

func await(rs ...runner) error {
	stop := make(chan struct{})
	g := &run.Group{}
	for i := range rs {
		r := rs[i] // https://golang.org/doc/faq#closures_and_goroutines
		g.Add(func() error { r.Run(stop); return nil }, func(err error) { close(stop) })
	}
	return g.Run()
}

type httpRunner struct {
	l string
	h map[string]http.Handler
}

func (r *httpRunner) Run(stop <-chan struct{}) {
	rt := httprouter.New()
	for path, handler := range r.h {
		rt.Handler("GET", path, handler)
	}

	s := &http.Server{Addr: r.l, Handler: rt}
	ctx, cancel := context.WithTimeout(context.Background(), 0*time.Second)
	go func() {
		<-stop
		s.Shutdown(ctx) // nolint:gosec
	}()
	s.ListenAndServe() // nolint:gosec
	cancel()
}

// Many Kubernetes client things depend on glog. glog gets sad when flag.Parse()
// is not called before it tries to emit a log line. flag.Parse() fights with
// kingpin.
func glogWorkaround() {
	os.Args = []string{os.Args[0], "-logtostderr=true", "-v=0", "-vmodule="}
	flag.Parse()
}
