package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-logr/logr"
	controlv1 "github.com/r7sa/k8s-routersync/api/v1"
	"github.com/r7sa/k8s-routersync/internal/netcraze"
)

type RouterType string

const (
	RouterTypeNetCraze RouterType = "netcraze"
)

const (
	POLL_INTERVAL = 30 * time.Second
)

type PoolKey struct {
	RouterType RouterType
	Address    string
}

type ScheduleInfo interface {
	FetchSchedules(ctx context.Context) error
	IsScheduleActive(ctx context.Context, name string) (bool, error)
}

type RouterSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RouterPool map[PoolKey]ScheduleInfo
}

func (r *RouterSyncReconciler) getUserPassFromSecret(ctx context.Context, namespace, secretRef string) (string, string, error) {
	log := logf.FromContext(ctx)

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: namespace, Name: secretRef}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		log.Error(err, "Secret not found", "name", secretRef)
		return "", "", err
	}
	user, ok := secret.Data["username"]
	if !ok {
		err := fmt.Errorf("username not found in secret")
		log.Error(err, "username not found in secret", "name", secretRef)
		return "", "", err
	}
	pass, ok := secret.Data["password"]
	if !ok {
		err := fmt.Errorf("password not found in secret")
		log.Error(err, "password not found in secret", "name", secretRef)
		return "", "", err
	}
	return string(user), string(pass), nil
}

func (r *RouterSyncReconciler) getOrCreateClient(routerType RouterType, address string, user, pass string) (ScheduleInfo, error) {
	if r.RouterPool == nil {
		r.RouterPool = make(map[PoolKey]ScheduleInfo)
	}

	key := PoolKey{RouterType: routerType, Address: address}
	nc := r.RouterPool[key]
	if nc == nil {
		switch routerType {
		case "", RouterTypeNetCraze:
			nc = netcraze.NewClient(address, user, pass)
		default:
			return nil, fmt.Errorf("unsupported router type: %s", routerType)
		}
		r.RouterPool[key] = nc
	}
	return nc, nil
}

func (r *RouterSyncReconciler) collectDeployments(ctx context.Context) (*appsv1.DeploymentList, error) {
	deployments := &appsv1.DeploymentList{}
	err := r.List(
		ctx, deployments,
		// client.InNamespace(req.Namespace),
		client.MatchingLabels{"routersync.r7sa.github.io/auto-scale-enabled": "true"},
	)
	if err != nil {
		return nil, err
	}

	log := logf.FromContext(ctx)
	log.Info("Found deployments", "count", len(deployments.Items))

	return deployments, nil
}

func (r *RouterSyncReconciler) collectSchedules(
	ctx context.Context, deployments *appsv1.DeploymentList, nc ScheduleInfo,
) (map[string]bool, error) {
	err := nc.FetchSchedules(ctx)
	if err != nil {
		return nil, err
	}

	scheduleActive := map[string]bool{}
	for _, dep := range deployments.Items {
		scheduleName, ok := dep.Labels["routersync.r7sa.github.io/auto-scale-schedule"]
		if !ok {
			continue
		}
		scheduleActive[scheduleName] = false
	}

	for scheduleName := range scheduleActive {
		isActive, err := nc.IsScheduleActive(ctx, scheduleName)
		if err != nil {
			return nil, err
		}
		scheduleActive[scheduleName] = isActive
	}

	log := logf.FromContext(ctx)
	log.Info("Found schedules", "count", len(scheduleActive))

	return scheduleActive, nil
}

func (r *RouterSyncReconciler) scaleDeployments(
	ctx context.Context, deployments *appsv1.DeploymentList, scheduleActive map[string]bool,
) error {
	log := logf.FromContext(ctx)

	errs := []error{}
	for _, dep := range deployments.Items {
		targetReplicas, err := r.getTargetReplicas(dep, log, scheduleActive)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if *dep.Spec.Replicas != targetReplicas {
			dep.Spec.Replicas = &targetReplicas
			if err := r.Update(ctx, &dep); err != nil {
				errs = append(errs, err)
				log.Error(err, "Can't update deployment", "name", dep.Name)
				continue
			}
			log.Info("Deployment replicas set", "deployment", dep.Name, "replicas", targetReplicas)
		}
	}
	return errors.Join(errs...)
}

func (*RouterSyncReconciler) getTargetReplicas(
	dep appsv1.Deployment, log logr.Logger, scheduleActive map[string]bool,
) (int32, error) {
	scheduleName, ok := dep.Labels["routersync.r7sa.github.io/auto-scale-schedule"]
	if !ok {
		log.Info("No schedule name label on deployment, skipping", "deployment", dep.Name)
		return 0, fmt.Errorf("no schedule name label on deployment %s", dep.Name)
	}
	scheduleIsOn, ok := scheduleActive[scheduleName]
	if !ok {
		log.Info("No information for schedule for deployment, skipping", "deployment", dep.Name, "schedule", scheduleName)
		return 0, fmt.Errorf("no information for schedule %s for deployment %s", scheduleName, dep.Name)
	}

	if scheduleIsOn {
		replicas := int32(1)
		replicasStr, ok := dep.Labels["routersync.r7sa.github.io/auto-scale-on-replicas"]
		if ok {
			if _, err := fmt.Sscanf(replicasStr, "%d", &replicas); err != nil {
				log.Error(err, "Can't parse on replicas label", "deployment", dep.Name)
				return 0, err
			}
		}
		return replicas, nil
	} else {
		replicas := int32(0)
		replicasStr, ok := dep.Labels["routersync.r7sa.github.io/auto-scale-off-replicas"]
		if ok {
			if _, err := fmt.Sscanf(replicasStr, "%d", &replicas); err != nil {
				log.Error(err, "Can't parse off replicas label", "deployment", dep.Name)
				return 0, err
			}
		}
		return replicas, nil
	}
}

// +kubebuilder:rbac:groups=control.routersync.r7sa.github.io,resources=routersyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=control.routersync.r7sa.github.io,resources=routersyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=control.routersync.r7sa.github.io,resources=routersyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *RouterSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Starting reconciling", "name", req.Name)

	var rs controlv1.RouterSync
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{RequeueAfter: POLL_INTERVAL}, client.IgnoreNotFound(err)
	}

	user, pass, err := r.getUserPassFromSecret(ctx, rs.Namespace, rs.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	nc, err := r.getOrCreateClient(RouterType(rs.Spec.RouterType), rs.Spec.RouterAddress, user, pass)
	if err != nil {
		return ctrl.Result{}, err
	}

	deployments, err := r.collectDeployments(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	scheduleActive, err := r.collectSchedules(ctx, deployments, nc)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.scaleDeployments(ctx, deployments, scheduleActive)
	if err != nil {
		return ctrl.Result{}, err
	}

	rs.Status.LastCheck = time.Now().String()
	if err := r.Status().Update(ctx, &rs); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Done reconciling", "name", req.Name)

	return ctrl.Result{RequeueAfter: POLL_INTERVAL}, nil
}

func (r *RouterSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// return ctrl.NewControllerManagedBy(mgr).
	// 	For(&controlv1.RouterSync{}).
	// 	Owns(&appsv1.Deployment{}).
	// 	Named("routersync").
	// 	Complete(r)

	return ctrl.NewControllerManagedBy(mgr).
		For(&controlv1.RouterSync{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("routersync").
		Complete(r)
}
