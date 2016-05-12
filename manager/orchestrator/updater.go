package orchestrator

import (
	"reflect"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/log"
	"github.com/docker/swarm-v2/manager/state"
	"github.com/docker/swarm-v2/manager/state/store"
	"github.com/docker/swarm-v2/manager/state/watch"
)

// UpdateSupervisor supervises a set of updates. It's responsible for keeping track of updates,
// shutting them down and replacing them.
type UpdateSupervisor struct {
	store   *store.MemoryStore
	updates map[string]*Updater
	l       sync.Mutex
}

// NewUpdateSupervisor creates a new UpdateSupervisor.
func NewUpdateSupervisor(store *store.MemoryStore) *UpdateSupervisor {
	return &UpdateSupervisor{
		store:   store,
		updates: make(map[string]*Updater),
	}
}

// Update starts an Update of `tasks` belonging to `service` in the background and returns immediately.
// If an update for that service was already in progress, it will be cancelled before the new one starts.
func (u *UpdateSupervisor) Update(ctx context.Context, service *api.Service, tasks []*api.Task) {
	u.l.Lock()
	defer u.l.Unlock()

	id := service.ID

	if update, ok := u.updates[id]; ok {
		update.Cancel()
	}

	update := NewUpdater(u.store)
	u.updates[id] = update
	go func() {
		update.Run(ctx, service, tasks)
		u.l.Lock()
		if u.updates[id] == update {
			delete(u.updates, id)
		}
		u.l.Unlock()
	}()
}

// CancelAll cancels all current updates.
func (u *UpdateSupervisor) CancelAll() {
	u.l.Lock()
	defer u.l.Unlock()

	for _, update := range u.updates {
		update.Cancel()
	}
}

// Updater updates a set of tasks to a new version.
type Updater struct {
	store      *store.MemoryStore
	watchQueue *watch.Queue

	// stopChan signals to the state machine to stop running.
	stopChan chan struct{}
	// doneChan is closed when the state machine terminates.
	doneChan chan struct{}
}

// NewUpdater creates a new Updater.
func NewUpdater(store *store.MemoryStore) *Updater {
	return &Updater{
		store:      store,
		watchQueue: store.WatchQueue(),
		stopChan:   make(chan struct{}),
		doneChan:   make(chan struct{}),
	}
}

// Cancel cancels the current update immediately. It blocks until the cancellation is confirmed.
func (u *Updater) Cancel() {
	close(u.stopChan)
	<-u.doneChan
}

// Run starts the update and returns only once its complete or cancelled.
func (u *Updater) Run(ctx context.Context, service *api.Service, tasks []*api.Task) {
	defer close(u.doneChan)

	dirtyTasks := []*api.Task{}
	for _, t := range tasks {
		if service.Spec.GetContainer() == nil &&
			reflect.DeepEqual(t.GetContainer().Spec, api.ContainerSpec{}) {
			continue
		}

		if !reflect.DeepEqual(service.Spec.GetContainer(), &(t.GetContainer().Spec)) {
			dirtyTasks = append(dirtyTasks, t)
		}
	}
	// Abort immediately if all tasks are clean.
	if len(dirtyTasks) == 0 {
		return
	}

	parallelism := 0
	if service.Spec.Update != nil {
		parallelism = int(service.Spec.Update.Parallelism)
	}
	if parallelism == 0 {
		// TODO(aluzzardi): We could try to optimize unlimited parallelism by performing updates in a single
		// goroutine using a batch transaction.
		parallelism = len(dirtyTasks)
	}

	// Start the workers.
	taskQueue := make(chan *api.Task)
	wg := sync.WaitGroup{}
	wg.Add(parallelism)
	for i := 0; i < parallelism; i++ {
		go func() {
			u.worker(ctx, service, taskQueue)
			wg.Done()
		}()
	}

	for _, t := range dirtyTasks {
		// Wait for a worker to pick up the task or abort the update, whichever comes first.
		select {
		case <-u.stopChan:
			break

		case taskQueue <- t:
		}
	}

	close(taskQueue)
	wg.Wait()
}

func (u *Updater) worker(ctx context.Context, service *api.Service, queue <-chan *api.Task) {
	for t := range queue {
		updated := newTask(service, t.Instance)
		if service.Spec.Mode == api.ServiceModeFill {
			updated.NodeID = t.NodeID
		}

		if err := u.updateTask(ctx, t, updated); err != nil {
			log.G(ctx).WithError(err).WithField("task.id", t.ID).Error("update failed")
		}

		if service.Spec.Update != nil && service.Spec.Update.Delay != 0 {
			select {
			case <-time.After(service.Spec.Update.Delay):
			case <-u.stopChan:
				return
			}
		}
	}
}

func (u *Updater) updateTask(ctx context.Context, original, updated *api.Task) error {
	log.G(ctx).Debugf("replacing %s with %s", original.ID, updated.ID)
	// Kick off the watch before even creating the updated task. This is in order to avoid missing any event.
	taskUpdates, cancel := state.Watch(u.watchQueue, state.EventUpdateTask{
		Task:   &api.Task{ID: updated.ID},
		Checks: []state.TaskCheckFunc{state.TaskCheckID},
	})
	defer cancel()

	// Atomically create the updated task and bring down the old one.
	err := u.store.Update(func(tx store.Tx) error {
		t := store.GetTask(tx, original.ID)
		t.DesiredState = api.TaskStateDead
		if err := store.UpdateTask(tx, t); err != nil {
			return err
		}

		if err := store.CreateTask(tx, updated); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Wait for the task to come up.
	// TODO(aluzzardi): Consider adding a timeout here.
	for {
		select {
		case e := <-taskUpdates:
			updated = e.(state.EventUpdateTask).Task
			if updated.Status.State >= api.TaskStateRunning {
				return nil
			}
		case <-u.stopChan:
			return nil
		}
	}
}