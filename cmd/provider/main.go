/*
Copyright 2020 The Crossplane Authors.

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

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/feature"
	"go.uber.org/zap/zapcore"

	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"gopkg.in/alecthomas/kingpin.v2"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/upbound/provider-terraform/apis"
	"github.com/upbound/provider-terraform/apis/v1beta1"
	workspace "github.com/upbound/provider-terraform/internal/controller"
	"github.com/upbound/provider-terraform/internal/controller/features"
)

func main() {

	var (
		app                        = kingpin.New(filepath.Base(os.Args[0]), "Terraform support for Crossplane.").DefaultEnvars()
		debug                      = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		syncInterval               = app.Flag("sync", "Sync interval controls how often all resources will be double checked for drift.").Short('s').Default("1h").Duration()
		pollInterval               = app.Flag("poll", "Poll interval controls how often an individual resource should be checked for drift.").Default("10m").Duration()
		pollJitter                 = app.Flag("poll-jitter", "If non-zero, varies the poll interval by a random amount up to plus-or-minus this value.").Default("1m").Duration()
		timeout                    = app.Flag("timeout", "Controls how long Terraform processes may run before they are killed.").Default("20m").Duration()
		leaderElection             = app.Flag("leader-election", "Use leader election for the controller manager.").Short('l').Default("false").Envar("LEADER_ELECTION").Bool()
		maxReconcileRate           = app.Flag("max-reconcile-rate", "The maximum number of concurrent reconciliation operations.").Default("1").Int()
		namespace                  = app.Flag("namespace", "Namespace used to set as default scope in default secret store config.").Default("crossplane-system").Envar("POD_NAMESPACE").String()
		enableExternalSecretStores = app.Flag("enable-external-secret-stores", "Enable support for ExternalSecretStores.").Default("false").Envar("ENABLE_EXTERNAL_SECRET_STORES").Bool()
		shardCount                 = app.Flag("shard-total", "Total number of shards.").Default("1").Envar("SHARD_COUNT").Uint64()
		shardNodeID                = app.Flag("shard-node-id", "The node ID, if a statefulset this is automatically from hostname").Uint64()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	if shardNodeID == nil {
		nodeIdRegex := regexp.MustCompile(`-(\d+)`)
		matches := nodeIdRegex.FindStringSubmatch(os.Getenv("HOSTNAME"))
		if len(matches) == 2 {
			parsedID, err := strconv.ParseUint(matches[1], 10, 64)
			if err != nil {
				kingpin.FatalIfError(err, "Cannot parse node id from hostname")
			}
			shardNodeID = &parsedID
		}
	}

	zl := zap.New(zap.UseDevMode(*debug), UseISO8601())
	log := logging.NewLogrLogger(zl.WithName("provider-terraform"))
	if *debug {
		// The controller-runtime runs with a no-op logger by default. It is
		// *very* verbose even at info level, so we only provide it a real
		// logger when we're running in debug mode.
		ctrl.SetLogger(zl)
	}

	log.Debug("Starting",
		"sync-period", syncInterval.String(),
		"poll-interval", pollInterval.String(),
		"poll-jitter", pollJitter.String(),
		"max-reconcile-rate", *maxReconcileRate)

	cfg, err := ctrl.GetConfig()
	kingpin.FatalIfError(err, "Cannot get API server rest config")

	mgr, err := ctrl.NewManager(ratelimiter.LimitRESTConfig(cfg, *maxReconcileRate), ctrl.Options{
		Cache: cache.Options{
			SyncPeriod:           syncInterval,
			DefaultFieldSelector: fields.OneTermEqualSelector("", ""),
			// write a selector to select the UID by a hash, use a transform func to do the hash math
		},

		// controller-runtime uses both ConfigMaps and Leases for leader
		// election by default. Leases expire after 15 seconds, with a
		// 10 second renewal deadline. We've observed leader loss due to
		// renewal deadlines being exceeded when under high load - i.e.
		// hundreds of reconciles per second and ~200rps to the API
		// server. Switching to Leases only and longer leases appears to
		// alleviate this.
		LeaderElection:             *leaderElection,
		LeaderElectionID:           "crossplane-leader-election-provider-terraform",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaseDuration:              func() *time.Duration { d := 60 * time.Second; return &d }(),
		RenewDeadline:              func() *time.Duration { d := 50 * time.Second; return &d }(),
	})
	kingpin.FatalIfError(err, "Cannot create controller manager")

	kingpin.FatalIfError(apis.AddToScheme(mgr.GetScheme()), "Cannot add terraform APIs to scheme")

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobal(*maxReconcileRate),
		Features:                &feature.Flags{},
	}

	if *enableExternalSecretStores {
		o.Features.Enable(features.EnableAlphaExternalSecretStores)
		log.Info("Alpha feature enabled", "flag", features.EnableAlphaExternalSecretStores)

		// Ensure default store config exists.
		kingpin.FatalIfError(resource.Ignore(kerrors.IsAlreadyExists, mgr.GetClient().Create(context.Background(), &v1beta1.StoreConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default",
			},
			Spec: v1beta1.StoreConfigSpec{
				// NOTE(turkenh): We only set required spec and expect optional
				// ones to properly be initialized with CRD level default values.
				SecretStoreConfig: xpv1.SecretStoreConfig{
					DefaultScope: *namespace,
				},
			},
		})), "cannot create default store config")
	}

	kingpin.FatalIfError(workspace.Setup(mgr, o, *timeout, *pollJitter, *shardNodeID, *shardCount), "Cannot setup Workspace controllers")
	kingpin.FatalIfError(mgr.Start(ctrl.SetupSignalHandler()), "Cannot start controller manager")
}

// UseISO8601 sets the logger to use ISO8601 timestamp format
func UseISO8601() zap.Opts {
	return func(o *zap.Options) {
		o.TimeEncoder = zapcore.ISO8601TimeEncoder
	}
}
