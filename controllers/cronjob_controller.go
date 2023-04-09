/*
Copyright 2023.

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

package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron"
	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	batchv1 "tutorial.kubebuilder.io/project/api/v1"
)

var (
	scheduledTimeAnnotation = "batch.tutorial.kubebuilder.io/scheduled-at"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

type Clock interface {
	Now() time.Time
}

// +kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the current CronJob instance
	var instance batchv1.CronJob
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		log.Error(err, "unable to fetch CronJob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// List active child jobs of the current CronJob instance
	var childJobs kbatch.JobList
	listOpts := []client.ListOption{
		client.InNamespace(req.Namespace),
		client.MatchingFields{
			"jobOwnerKey": req.Name,
		},
	}
	if err := r.List(ctx, &childJobs, listOpts...); err != nil {
		log.Error(err, "unable to list child jobs")
		return ctrl.Result{}, err
	}

	var activeJobs, successfulJobs, failedJobs []*kbatch.Job
	var mostRecentTime *time.Time

	for _, job := range childJobs.Items {
		// segregate jobs based on type
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &job)
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &job)
		case kbatch.JobFailed:
			successfulJobs = append(failedJobs, &job)
		}
		// compute mostRecentTime from scheduledTime annotation value
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse scheduled time for job", "job", &job)
			continue
		}

		if scheduledTimeForJob == nil {
			continue
		}

		// if current childjob is to start later than mostRecentTime then update mostRecentTime
		if mostRecentTime == nil || mostRecentTime.Before(*scheduledTimeForJob) {
			mostRecentTime = scheduledTimeForJob
		}
	}

	if mostRecentTime != nil {
		instance.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		instance.Status.LastScheduleTime = nil
	}

	instance.Status.Active = nil
	for _, job := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, job)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", job)
			continue
		}

		instance.Status.Active = append(instance.Status.Active, *jobRef)
	}

	// log child job counts at verbosity 1
	log.V(1).Info("job count",
		"active jobs", len(activeJobs),
		"successful jobs", len(successfulJobs),
		"failed jobs", len(failedJobs),
	)

	// update the status subresource of the CR
	if err := r.Status().Update(ctx, &instance); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	// cleanup old jobs
	if instance.Spec.FailedJobsHistoryLimit != nil {
		if err := r.deleteOldJobs(ctx, failedJobs, *instance.Spec.FailedJobsHistoryLimit); err != nil {
			return ctrl.Result{}, err
		}
	}

	if instance.Spec.SuccessfulJobsHistoryLimit != nil {
		if err := r.deleteOldJobs(ctx, successfulJobs, *instance.Spec.SuccessfulJobsHistoryLimit); err != nil {
			return ctrl.Result{}, err
		}
	}

	// skip creation of new jobs if CR has suspend: true
	if instance.Spec.Suspend != nil && *instance.Spec.Suspend {
		log.V(1).Info("cronjob suspended, skipping")
		return ctrl.Result{}, nil
	}

	// calculate the next time the job has to be created
	missedRun, nextRun, err := getNextSchedule(&instance, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out CronJob schedule")
		return ctrl.Result{}, err
	}

	// prep requeue request until next job
	scheduledResult := ctrl.Result{
		RequeueAfter: nextRun.Sub(r.Now()),
	}
	log = log.WithValues("now", r.Now(), "next run", nextRun)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.CronJob{}).
		Complete(r)
}

func (r *CronJobReconciler) deleteOldJobs(ctx context.Context, jobs []*kbatch.Job, historyLimit int32) error {
	log := log.FromContext(ctx)

	// sort in ascending order of start time
	sortJobsOnStartTime(jobs)

	for i, job := range jobs {
		if int32(i) >= int32(len(jobs))-historyLimit {
			break
		}

		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to delete old job", "job", job)
			return err
		} else {
			log.V(0).Info("deleted old job", "job", job)
		}
	}

	return nil
}

func isJobFinished(job *kbatch.Job) (bool, kbatch.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
			// job is either completed or failed, and the status is true (finished)
			return true, c.Type
		}
	}

	return false, ""
}

func getScheduledTimeForJob(job *kbatch.Job) (*time.Time, error) {
	timeRaw := job.GetAnnotations()[scheduledTimeAnnotation]
	if len(timeRaw) == 0 {
		return nil, nil
	}

	timeParsed, err := time.Parse(time.RFC3339, timeRaw)
	if err != nil {
		return nil, err
	}

	return &timeParsed, nil
}

func sortJobsOnStartTime(jobs []*kbatch.Job) {
	sort.Slice(jobs, func(i, j int) bool {
		firstJob, secondJob := jobs[i], jobs[j]

		if firstJob.Status.StartTime == nil {
			return secondJob.Status.StartTime != nil
		}
		return firstJob.Status.StartTime.Before(secondJob.Status.StartTime)
	})
}

func getNextSchedule(cronJob *batchv1.CronJob, now time.Time) (lastMissed, next time.Time, err error) {
	sched, err := cron.ParseStandard(cronJob.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %v", cronJob.Spec.Schedule, err)
	}

	// start from last observed run time
	var earliestTime time.Time
	if cronJob.Status.LastScheduleTime != nil {
		earliestTime = cronJob.Status.LastScheduleTime.Time
	} else {
		earliestTime = cronJob.GetObjectMeta().GetCreationTimestamp().Time
	}

	if cronJob.Spec.StartingDeadlineSeconds != nil {
		// controller won't schedule anything below this point
		schedulingDeadline := now.Add(-time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds))

		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}
	if earliestTime.After(now) {
		return time.Time{}, sched.Next(now), nil
	}

	starts := 0
	for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		lastMissed = t
		starts++
		if starts > 100 {
			// we can't get the most recent times so just return an empty slice
			return time.Time{}, time.Time{}, fmt.Errorf("too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew")
		}
	}
	return lastMissed, sched.Next(now), nil
}
