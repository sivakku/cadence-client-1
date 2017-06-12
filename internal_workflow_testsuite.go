// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cadence

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/facebookgo/clock"
	"github.com/uber/tchannel-go/thrift"
	"go.uber.org/atomic"
	m "go.uber.org/cadence/.gen/go/cadence"
	"go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/common"
	"go.uber.org/cadence/mock"
	"go.uber.org/cadence/mocks"
	"go.uber.org/zap"
)

const (
	defaultTestTaskList   = "default-test-tasklist"
	defaultTestWorkflowID = "default-test-workflow-id"
	defaultTestRunID      = "default-test-run-id"
)

type (
	testTimerHandle struct {
		callback       resultHandler
		timer          *clock.Timer
		wallTimer      *clock.Timer
		duration       time.Duration
		mockTimeToFire time.Time
		wallTimeToFire time.Time
		timerID        int
	}

	testActivityHandle struct {
		callback     resultHandler
		activityType string
	}

	testChildWorkflowHandle struct {
		env *testWorkflowEnvironmentImpl
	}

	testCallbackHandle struct {
		callback          func()
		startDecisionTask bool // start a new decision task after callback() is handled.
	}

	activityExecutorWrapper struct {
		*activityExecutor
		env *testWorkflowEnvironmentImpl
	}

	workflowExecutorWrapper struct {
		*workflowExecutor
		env *testWorkflowEnvironmentImpl
	}

	mockWrapper struct {
		mock       *mock.Mock
		name       string
		fn         interface{}
		isWorkflow bool
	}

	taskListSpecificActivity struct {
		fn        interface{}
		taskLists map[string]struct{}
	}

	// testWorkflowEnvironmentImpl is the environment that runs the workflow/activity unit tests.
	testWorkflowEnvironmentImpl struct {
		testSuite                  *WorkflowTestSuite
		parentEnv                  *testWorkflowEnvironmentImpl
		overrodeWorkflows          map[string]interface{} // map of registered-fnName -> fakeWorkflowFn
		overrodeActivities         map[string]interface{} // map of registered-fnName -> fakeActivityFn
		taskListSpecificActivities map[string]*taskListSpecificActivity

		mock          *mock.Mock
		service       m.TChanWorkflowService
		workerOptions WorkerOptions
		logger        *zap.Logger
		mockClock     *clock.Mock
		wallClock     clock.Clock

		workflowInfo          *WorkflowInfo
		workflowDef           workflowDefinition
		counterID             int
		workflowCancelHandler func()
		signalHandler         func(name string, input []byte)

		locker         *sync.Mutex
		activities     map[string]*testActivityHandle
		timers         map[string]*testTimerHandle
		childWorkflows map[string]*testChildWorkflowHandle

		runningCount atomic.Int32

		onActivityStartedListener        func(activityInfo *ActivityInfo, ctx context.Context, args EncodedValues)
		onActivityCompletedListener      func(activityInfo *ActivityInfo, result EncodedValue, err error)
		onActivityCanceledListener       func(activityInfo *ActivityInfo)
		onActivityHeartbeatListener      func(activityInfo *ActivityInfo, details EncodedValues)
		onChildWorkflowStartedListener   func(workflowInfo *WorkflowInfo, ctx Context, args EncodedValues)
		onChildWorkflowCompletedListener func(workflowInfo *WorkflowInfo, result EncodedValue, err error)
		onChildWorkflowCanceledListener  func(workflowInfo *WorkflowInfo)
		onTimerScheduledListener         func(timerID string, duration time.Duration)
		onTimerFiredListener             func(timerID string)
		onTimerCancelledListener         func(timerID string)

		callbackChannel chan testCallbackHandle
		testTimeout     time.Duration
		isTestCompleted bool
		testResult      EncodedValue
		testError       error
		panicHandler    func(err error)
	}
)

func newTestWorkflowEnvironmentImpl(s *WorkflowTestSuite) *testWorkflowEnvironmentImpl {
	env := &testWorkflowEnvironmentImpl{
		testSuite: s,
		logger:    s.logger,

		overrodeWorkflows:          make(map[string]interface{}),
		overrodeActivities:         make(map[string]interface{}),
		taskListSpecificActivities: make(map[string]*taskListSpecificActivity),

		workflowInfo: &WorkflowInfo{
			WorkflowExecution: WorkflowExecution{
				ID:    defaultTestWorkflowID,
				RunID: defaultTestRunID,
			},
			WorkflowType: WorkflowType{Name: "workflow-type-not-specified"},
			TaskListName: defaultTestTaskList,

			ExecutionStartToCloseTimeoutSeconds: 1,
			TaskStartToCloseTimeoutSeconds:      1,
		},

		locker:          &sync.Mutex{},
		activities:      make(map[string]*testActivityHandle),
		childWorkflows:  make(map[string]*testChildWorkflowHandle),
		timers:          make(map[string]*testTimerHandle),
		callbackChannel: make(chan testCallbackHandle, 1000),
		testTimeout:     time.Second * 3,
	}

	if env.logger == nil {
		logger, _ := zap.NewDevelopment()
		env.logger = logger
	}

	// setup mock service
	mockService := new(mocks.TChanWorkflowService)
	mockHeartbeatFn := func(c thrift.Context, r *shared.RecordActivityTaskHeartbeatRequest) error {
		activityID := string(r.TaskToken)
		env.locker.Lock() // need lock as this is running in activity worker's goroutinue
		activityHandle, ok := env.activities[activityID]
		env.locker.Unlock()
		if !ok {
			env.logger.Debug("RecordActivityTaskHeartbeat: ActivityID not found, could be already completed or cancelled.",
				zap.String(tagActivityID, activityID))
			return shared.NewEntityNotExistsError()
		}
		activityInfo := env.getActivityInfo(activityID, activityHandle.activityType)
		env.postCallback(func() {
			if env.onActivityHeartbeatListener != nil {
				env.onActivityHeartbeatListener(activityInfo, EncodedValues(r.Details))
			}
		}, false)

		env.logger.Debug("RecordActivityTaskHeartbeat", zap.String(tagActivityID, activityID))
		return nil
	}

	mockService.On("RecordActivityTaskHeartbeat", mock.Anything, mock.Anything).Return(
		&shared.RecordActivityTaskHeartbeatResponse{CancelRequested: common.BoolPtr(false)},
		mockHeartbeatFn)
	env.service = mockService

	if env.workerOptions.Logger == nil {
		env.workerOptions.Logger = env.logger
	}

	env.mockClock = clock.NewMock()
	env.wallClock = clock.New()

	return env
}

func (env *testWorkflowEnvironmentImpl) newTestWorkflowEnvironmentForChild(options *workflowOptions) *testWorkflowEnvironmentImpl {
	// create a new test env
	childEnv := newTestWorkflowEnvironmentImpl(env.testSuite)
	childEnv.parentEnv = env

	// tests do not have access to child test env, we need to copy over setup config from parent to child
	childEnv.mock = env.mock
	childEnv.testTimeout = env.testTimeout
	for k, v := range env.overrodeWorkflows {
		childEnv.overrodeWorkflows[k] = v
	}
	for k, v := range env.overrodeActivities {
		childEnv.overrodeActivities[k] = v
	}
	for k, v := range env.taskListSpecificActivities {
		childEnv.taskListSpecificActivities[k] = v
	}

	if options.workflowID == "" {
		options.workflowID = env.workflowInfo.WorkflowExecution.RunID + "_" + getStringID(env.nextID())
	}
	// set workflow info data for child workflow
	childEnv.workflowInfo.WorkflowExecution.ID = options.workflowID
	childEnv.workflowInfo.WorkflowExecution.RunID = options.workflowID + "_RunID"
	childEnv.workflowInfo.Domain = *options.domain
	childEnv.workflowInfo.TaskListName = *options.taskListName
	childEnv.workflowInfo.ExecutionStartToCloseTimeoutSeconds = *options.executionStartToCloseTimeoutSeconds
	childEnv.workflowInfo.TaskStartToCloseTimeoutSeconds = *options.taskStartToCloseTimeoutSeconds

	workflowID := childEnv.workflowInfo.WorkflowExecution.ID
	env.childWorkflows[workflowID] = &testChildWorkflowHandle{env: childEnv}

	return childEnv
}

func (env *testWorkflowEnvironmentImpl) setActivityTaskList(tasklist string, activityFns ...interface{}) {
	for _, activityFn := range activityFns {
		env.testSuite.RegisterActivity(activityFn)
		fnName := getFunctionName(activityFn)
		taskListActivity, ok := env.taskListSpecificActivities[fnName]
		if !ok {
			taskListActivity = &taskListSpecificActivity{fn: activityFn, taskLists: make(map[string]struct{})}
			env.taskListSpecificActivities[fnName] = taskListActivity
		}
		taskListActivity.taskLists[tasklist] = struct{}{}
	}
}

func (env *testWorkflowEnvironmentImpl) executeWorkflow(workflowFn interface{}, args ...interface{}) {
	s := env.testSuite
	var workflowType string
	fnType := reflect.TypeOf(workflowFn)
	switch fnType.Kind() {
	case reflect.String:
		workflowType = workflowFn.(string)
	case reflect.Func:
		// auto register workflow if it is not already registered
		fnName := getFunctionName(workflowFn)
		if _, ok := s.hostEnv.getWorkflowFn(fnName); !ok {
			s.RegisterWorkflow(workflowFn)
		}
		workflowType = getFunctionName(workflowFn)
	default:
		panic("unsupported workflowFn")
	}

	input, err := s.hostEnv.encodeArgs(args)
	if err != nil {
		panic(err)
	}
	env.executeWorkflowInternal(workflowType, input)
}

func (env *testWorkflowEnvironmentImpl) workflowFactory(workflowType WorkflowType) (workflow, error) {
	factory := env.testSuite.hostEnv.newRegisteredWorkflowFactory()
	wf, err := factory(workflowType)
	if err != nil {
		factory = getHostEnvironment().newRegisteredWorkflowFactory()
		wf, err = factory(workflowType)
		if err != nil {
			return nil, err
		}
	}

	return &workflowExecutorWrapper{workflowExecutor: wf.(*workflowExecutor), env: env}, nil
}

func (env *testWorkflowEnvironmentImpl) executeWorkflowInternal(workflowType string, input []byte) {
	env.workflowInfo.WorkflowType.Name = workflowType
	factory := getWorkflowDefinitionFactory(env.workflowFactory)
	workflowDefinition, err := factory(env.workflowInfo.WorkflowType)
	if err != nil {
		panic(err)
	}
	env.workflowDef = workflowDefinition
	env.workflowDef.Execute(env, input)
	env.startMainLoop()
}

func (env *testWorkflowEnvironmentImpl) executeActivity(
	activityFn interface{}, args ...interface{}) (EncodedValue, error) {
	fnName := getFunctionName(activityFn)

	input, err := getHostEnvironment().encodeArgs(args)
	if err != nil {
		panic(err)
	}

	task := newTestActivityTask(
		defaultTestWorkflowID,
		defaultTestRunID,
		"0",
		fnName,
		input,
	)

	// ensure activityFn is registered to defaultTestTaskList
	env.testSuite.RegisterActivity(activityFn)
	taskHandler := env.newTestActivityTaskHandler(defaultTestTaskList)
	result, err := taskHandler.Execute(task)
	if err != nil {
		panic(err)
	}
	switch request := result.(type) {
	case *shared.RespondActivityTaskCanceledRequest:
		return nil, NewCanceledError(request.Details)
	case *shared.RespondActivityTaskFailedRequest:
		return nil, NewErrorWithDetails(*request.Reason, request.Details)
	case *shared.RespondActivityTaskCompletedRequest:
		return EncodedValue(request.Result_), nil
	default:
		// will never happen
		return nil, fmt.Errorf("unsupported respond type %T", result)
	}
}

func validateOverrideFunction(fn, fakeFn interface{}, isWorkflow bool) {
	// verify both functions are valid workflow func
	actualFnType := reflect.TypeOf(fn)
	if err := validateFnFormat(actualFnType, isWorkflow); err != nil {
		panic(err)
	}
	fakeFnType := reflect.TypeOf(fakeFn)
	if err := validateFnFormat(fakeFnType, isWorkflow); err != nil {
		panic(err)
	}

	// verify function signatures are the same.
	if actualFnType != fakeFnType {
		panic(fmt.Sprintf("override failed, expected %v, but got %v.", actualFnType, fakeFnType))
	}
}

func (env *testWorkflowEnvironmentImpl) overrideActivity(activityFn, fakeActivityFn interface{}) {
	validateOverrideFunction(activityFn, fakeActivityFn, false)
	fnName := getFunctionName(activityFn)
	env.overrodeActivities[fnName] = fakeActivityFn
}

func (env *testWorkflowEnvironmentImpl) overrideWorkflow(workflowFn, fakeWorkflowFn interface{}) {
	validateOverrideFunction(workflowFn, fakeWorkflowFn, true)
	fnName := getFunctionName(workflowFn)
	env.overrodeWorkflows[fnName] = fakeWorkflowFn
}

// startDecisionTask will trigger OnDecisionTaskStart() on the workflow which will execute the dispatcher until all
// coroutinues are blocked. This method is only necessary when you disable the auto start decision task to have full
// control of the workflow execution on when to start a decision task.
func (env *testWorkflowEnvironmentImpl) startDecisionTask() {
	// post an empty callback to event loop, and request OnDecisionTaskStarted to be triggered after that empty callback
	env.postCallback(func() {}, true /* to start decision task */)
}

func (env *testWorkflowEnvironmentImpl) startMainLoop() {
	for {
		// use non-blocking-select to check if there is anything pending in the main thread.
		select {
		case c := <-env.callbackChannel:
			// this will drain the callbackChannel
			env.processCallback(c)
		default:
			// nothing to process, main thread is blocked at this moment, now check if we should auto fire next timer
			if !env.autoFireNextTimer() {
				if env.isTestCompleted {
					return
				}

				// no timer to fire, wait for things to do or timeout.
				select {
				case c := <-env.callbackChannel:
					env.processCallback(c)
				case <-time.After(env.testTimeout):
					// not able to complete workflow within test timeout, workflow likely stuck somewhere,
					// check workflow stack for more details.
					panicMsg := fmt.Sprintf("test timeout: %v, workflow stack: %v",
						env.testTimeout, env.workflowDef.StackTrace())
					if env.panicHandler != nil {
						env.panicHandler(errors.New(panicMsg))
						return
					} else {
						panic(panicMsg)
					}
				}
			}
		}
	}
}

func (env *testWorkflowEnvironmentImpl) registerDelayedCallback(f func(), delayDuration time.Duration) {
	env.postCallback(func() {
		env.NewTimer(delayDuration, func(result []byte, err error) {
			f()
		})
	}, true)
}

func (env *testWorkflowEnvironmentImpl) processCallback(c testCallbackHandle) {
	c.callback()
	if c.startDecisionTask && !env.isTestCompleted {
		env.workflowDef.OnDecisionTaskStarted() // this will execute dispatcher
	}
}

func (env *testWorkflowEnvironmentImpl) autoFireNextTimer() bool {
	if len(env.timers) == 0 {
		return false
	}

	// find next timer
	var nextTimer *testTimerHandle
	for _, t := range env.timers {
		if nextTimer == nil {
			nextTimer = t
		} else if t.mockTimeToFire.Before(nextTimer.mockTimeToFire) ||
			(t.mockTimeToFire.Equal(nextTimer.mockTimeToFire) && t.timerID < nextTimer.timerID) {
			nextTimer = t
		}
	}

	// function to fire timer
	fireTimer := func(th *testTimerHandle) {
		skipDuration := th.mockTimeToFire.Sub(env.mockClock.Now())
		env.logger.Debug("Auto fire timer",
			zap.Int(tagTimerID, th.timerID),
			zap.Duration("TimerDuration", th.duration),
			zap.Duration("TimeSkipped", skipDuration))

		// Move mockClock forward, this will fire the timer, and the timer callback will remove timer from timers.
		env.mockClock.Add(skipDuration)
	}

	// fire timer if there is no running activity
	if env.runningCount.Load() == 0 {
		if nextTimer.wallTimer != nil {
			nextTimer.wallTimer.Stop()
			nextTimer.wallTimer = nil
		}
		fireTimer(nextTimer)
		return true
	}

	durationToFire := nextTimer.mockTimeToFire.Sub(env.mockClock.Now())
	wallTimeToFire := env.wallClock.Now().Add(durationToFire)

	if nextTimer.wallTimer != nil && nextTimer.wallTimeToFire.Before(wallTimeToFire) {
		// nextTimer already set, meaning we already have a wall clock timer for the nextTimer setup earlier. And the
		// previously scheduled wall time to fire is before the wallTimeToFire calculated this time. This could happen
		// if workflow was blocked while there was activity running, and when that activity completed, there are some
		// other activities still running while the nextTimer is still that same nextTimer. In that case, we should not
		// reset the wall time to fire for the nextTimer.
		return false
	}
	if nextTimer.wallTimer != nil {
		// wallTimer was scheduled, but the wall time to fire should be earlier based on current calculation.
		nextTimer.wallTimer.Stop()
	}

	// there is running activities, we would fire next timer only if wall time passed by nextTimer duration.
	nextTimer.wallTimeToFire, nextTimer.wallTimer = wallTimeToFire, env.wallClock.AfterFunc(durationToFire, func() {
		// make sure it is running in the main loop
		env.postCallback(func() {
			// now fire the timer if it is not already fired.
			if timerHandle, ok := env.timers[getStringID(nextTimer.timerID)]; ok {
				fireTimer(timerHandle)
			}
		}, true)
	})

	return false
}

func (env *testWorkflowEnvironmentImpl) postCallback(cb func(), startDecisionTask bool) {
	env.callbackChannel <- testCallbackHandle{callback: cb, startDecisionTask: startDecisionTask}
}

func (env *testWorkflowEnvironmentImpl) RequestCancelActivity(activityID string) {
	handle, ok := env.activities[activityID]
	if !ok {
		env.logger.Debug("RequestCancelActivity failed, Activity not exists or already completed.", zap.String(tagActivityID, activityID))
		return
	}
	activityInfo := env.getActivityInfo(activityID, handle.activityType)
	env.logger.Debug("RequestCancelActivity", zap.String(tagActivityID, activityID))
	delete(env.activities, activityID)
	env.postCallback(func() {
		handle.callback(nil, NewCanceledError())
		if env.onActivityCanceledListener != nil {
			env.onActivityCanceledListener(activityInfo)
		}
	}, true)
}

// RequestCancelTimer request to cancel timer on this testWorkflowEnvironmentImpl.
func (env *testWorkflowEnvironmentImpl) RequestCancelTimer(timerID string) {
	env.logger.Debug("RequestCancelTimer", zap.String(tagTimerID, timerID))
	timerHandle, ok := env.timers[timerID]
	if !ok {
		env.logger.Debug("RequestCancelTimer failed, TimerID not exists.", zap.String(tagTimerID, timerID))
		return
	}

	delete(env.timers, timerID)
	timerHandle.timer.Stop()
	env.postCallback(func() {
		timerHandle.callback(nil, NewCanceledError())
		if env.onTimerCancelledListener != nil {
			env.onTimerCancelledListener(timerID)
		}
	}, true)
}

func (env *testWorkflowEnvironmentImpl) Complete(result []byte, err error) {
	if env.isTestCompleted {
		env.logger.Debug("Workflow already completed.")
		return
	}
	env.isTestCompleted = true
	env.testResult = EncodedValue(result)
	env.testError = err

	if err == ErrCanceled && env.workflowCancelHandler != nil {
		env.workflowCancelHandler()
	}
}

func (env *testWorkflowEnvironmentImpl) CompleteActivity(taskToken []byte, result interface{}, err error) error {
	if taskToken == nil {
		return errors.New("nil task token provided")
	}
	var data []byte
	if result != nil {
		var encodeErr error
		data, encodeErr = getHostEnvironment().encodeArg(result)
		if encodeErr != nil {
			return encodeErr
		}
	}

	activityID := string(taskToken)
	env.postCallback(func() {
		activityHandle, ok := env.activities[activityID]
		if !ok {
			env.logger.Debug("CompleteActivity: ActivityID not found, could be already completed or cancelled.",
				zap.String(tagActivityID, activityID))
			return
		}
		request := convertActivityResultToRespondRequest("test-identity", taskToken, data, err)
		env.handleActivityResult(activityID, request, activityHandle.activityType)
	}, false /* do not auto schedule decision task, because activity might be still pending */)

	return nil
}

func (env *testWorkflowEnvironmentImpl) GetLogger() *zap.Logger {
	return env.logger
}

func (env *testWorkflowEnvironmentImpl) ExecuteActivity(parameters executeActivityParameters, callback resultHandler) *activityInfo {
	var activityID string
	if parameters.ActivityID == nil || *parameters.ActivityID == "" {
		activityID = getStringID(env.nextID())
	} else {
		activityID = *parameters.ActivityID
	}
	activityInfo := &activityInfo{activityID: activityID}
	task := newTestActivityTask(
		defaultTestWorkflowID,
		defaultTestRunID,
		activityInfo.activityID,
		parameters.ActivityType.Name,
		parameters.Input,
	)

	taskHandler := env.newTestActivityTaskHandler(parameters.TaskListName)
	activityHandle := &testActivityHandle{callback: callback, activityType: parameters.ActivityType.Name}

	// locker is needed to prevent race condition between dispatcher loop goroutinue and activity worker goroutinues.
	// The activity workers could call into Heartbeat which by default is mocked in this test suite. The mock needs to
	// access s.activities map, that could cause data race warning.
	env.locker.Lock()
	env.activities[activityInfo.activityID] = activityHandle
	env.locker.Unlock()

	env.runningCount.Inc()

	// activity runs in separate goroutinue outside of workflow dispatcher
	go func() {
		result, err := taskHandler.Execute(task)
		if err != nil {
			panic(err)
		}
		// post activity result to workflow dispatcher
		env.postCallback(func() {
			env.handleActivityResult(activityInfo.activityID, result, parameters.ActivityType.Name)
		}, false /* do not auto schedule decision task, because activity might be still pending */)
		env.runningCount.Dec()
	}()

	return activityInfo
}

func (env *testWorkflowEnvironmentImpl) handleActivityResult(activityID string, result interface{}, activityType string) {
	env.logger.Debug(fmt.Sprintf("handleActivityResult: %T.", result),
		zap.String(tagActivityID, activityID), zap.String(tagActivityType, activityType))
	activityInfo := env.getActivityInfo(activityID, activityType)
	if result == nil {
		// In case activity returns ErrActivityResultPending, the respond will be nil, and we don't need to do anything.
		// Activity will need to complete asynchronously using CompleteActivity().
		if env.onActivityCompletedListener != nil {
			env.onActivityCompletedListener(activityInfo, nil, ErrActivityResultPending)
		}
		return
	}

	// this is running in dispatcher
	activityHandle, ok := env.activities[activityID]
	if !ok {
		env.logger.Debug("handleActivityResult: ActivityID not exists, could be already completed or cancelled.",
			zap.String(tagActivityID, activityID))
		return
	}

	delete(env.activities, activityID)

	var blob []byte
	var err error

	switch request := result.(type) {
	case *shared.RespondActivityTaskCanceledRequest:
		err = NewCanceledError(request.Details)
		activityHandle.callback(nil, err)
	case *shared.RespondActivityTaskFailedRequest:
		err = NewErrorWithDetails(*request.Reason, request.Details)
		activityHandle.callback(nil, err)
	case *shared.RespondActivityTaskCompletedRequest:
		blob = request.Result_
		activityHandle.callback(blob, nil)
	default:
		panic(fmt.Sprintf("unsupported respond type %T", result))
	}

	if env.onActivityCompletedListener != nil {
		env.onActivityCompletedListener(activityInfo, EncodedValue(blob), err)
	}

	env.startDecisionTask()
}

// Execute executes the activity code.
func (a *activityExecutorWrapper) Execute(ctx context.Context, input []byte) ([]byte, error) {
	activityInfo := GetActivityInfo(ctx)
	if a.env.onActivityStartedListener != nil {
		a.env.postCallback(func() {
			a.env.onActivityStartedListener(&activityInfo, ctx, EncodedValues(input))
		}, false)
	}

	m := &mockWrapper{mock: a.env.mock, name: a.name, fn: a.fn, isWorkflow: false}
	if mockRet := m.getMockReturn(ctx, input); mockRet != nil {
		return m.executeMock(ctx, input, mockRet)
	}

	if fakeFn, ok := a.env.overrodeActivities[a.name]; ok {
		executor := &activityExecutor{name: a.name, fn: fakeFn}
		return executor.Execute(ctx, input)
	}

	return a.activityExecutor.Execute(ctx, input)
}

// Execute executes the workflow code.
func (w *workflowExecutorWrapper) Execute(ctx Context, input []byte) (result []byte, err error) {
	parentEnv := w.env.parentEnv
	if parentEnv != nil && parentEnv.onChildWorkflowStartedListener != nil {
		parentEnv.postCallback(func() {
			parentEnv.onChildWorkflowStartedListener(GetWorkflowInfo(ctx), ctx, EncodedValues(input))
		}, false)
	}

	m := &mockWrapper{mock: w.env.mock, name: w.name, fn: w.fn, isWorkflow: true}
	if mockRet := m.getMockReturn(ctx, input); mockRet != nil {
		return m.executeMock(ctx, input, mockRet)
	}

	if fakeFn, ok := w.env.overrodeWorkflows[w.name]; ok {
		executor := &workflowExecutor{name: w.name, fn: fakeFn}
		return executor.Execute(ctx, input)
	}

	// no mock, no override, so call the actual workflow
	return w.workflowExecutor.Execute(ctx, input)
}

func (m *mockWrapper) getMockReturn(ctx interface{}, input []byte) mock.Arguments {
	if m.mock == nil {
		// no mock
		return nil
	}

	// check if we have mock setup
	fnType := reflect.TypeOf(m.fn)
	reflectArgs, err := getHostEnvironment().decodeArgs(fnType, input)
	if err != nil {
		panic(err)
	}
	realArgs := []interface{}{}
	if fnType.NumIn() > 0 {
		if (!m.isWorkflow && isActivityContext(fnType.In(0))) ||
			(m.isWorkflow && isWorkflowContext(fnType.In(0))) {
			realArgs = append(realArgs, ctx)
		}
	}
	for _, arg := range reflectArgs {
		realArgs = append(realArgs, arg.Interface())
	}
	found, _ := m.mock.FindExpectedCall(m.name, realArgs...)

	if found < 0 {
		// mock call not found
		return nil
	}

	return m.mock.MethodCalled(m.name, realArgs...)
}

func (m *mockWrapper) executeMock(ctx interface{}, input []byte, mockRet mock.Arguments) ([]byte, error) {
	fnName := m.name
	mockRetLen := len(mockRet)
	if mockRetLen == 0 {
		panic(fmt.Sprintf("mock of %v has no returns", fnName))
	}

	fnType := reflect.TypeOf(m.fn)
	// check if mock returns function which must match to the actual function.
	mockFn := mockRet.Get(0)
	mockFnType := reflect.TypeOf(mockFn)
	if mockFnType != nil && mockFnType.Kind() == reflect.Func {
		if mockFnType != fnType {
			panic(fmt.Sprintf("mock of %v has incorrect return function, expected %v, but actual is %v",
				fnName, fnType, mockFnType))
		}
		// we found a mock function that matches to actual function, so call that mockFn
		if m.isWorkflow {
			executor := &workflowExecutor{name: fnName, fn: mockFn}
			return executor.Execute(ctx.(Context), input)
		}

		executor := &activityExecutor{name: fnName, fn: mockFn}
		return executor.Execute(ctx.(context.Context), input)
	}

	// check if mockRet have same types as function's return types
	if mockRetLen != fnType.NumOut() {
		panic(fmt.Sprintf("mock of %v has incorrect number of returns, expected %d, but actual is %d",
			fnName, fnType.NumOut(), mockRetLen))
	}
	// we already verified function either has 1 return value (error) or 2 return values (result, error)
	var retErr error
	mockErr := mockRet[mockRetLen-1] // last mock return must be error
	if mockErr == nil {
		retErr = nil
	} else if err, ok := mockErr.(error); ok {
		retErr = err
	} else {
		panic(fmt.Sprintf("mock of %v has incorrect return type, expected error, but actual is %T (%v)",
			fnName, mockErr, mockErr))
	}

	switch mockRetLen {
	case 1:
		return nil, retErr
	case 2:
		expectedType := fnType.Out(0)
		mockResult := mockRet[0]
		if mockResult == nil {
			switch expectedType.Kind() {
			case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Array:
				// these are supported nil-able types. (reflect.Chan, reflect.Func are nil-able, but not supported)
				return nil, retErr
			default:
				panic(fmt.Sprintf("mock of %v has incorrect return type, expected %v, but actual is %T (%v)",
					fnName, expectedType, mockResult, mockResult))
			}
		} else {
			if !reflect.TypeOf(mockResult).AssignableTo(expectedType) {
				panic(fmt.Sprintf("mock of %v has incorrect return type, expected %v, but actual is %T (%v)",
					fnName, expectedType, mockResult, mockResult))
			}
			result, encodeErr := getHostEnvironment().encodeArg(mockResult)
			if encodeErr != nil {
				panic(fmt.Sprintf("encode result from mock of %v failed: %v", fnName, encodeErr))
			}
			return result, retErr
		}
	default:
		// this will never happen, panic just in case
		panic("mock should either have 1 return value (error) or 2 return values (result, error)")
	}
}

func (env *testWorkflowEnvironmentImpl) newTestActivityTaskHandler(taskList string) ActivityTaskHandler {
	wOptions := fillWorkerOptionsDefaults(env.workerOptions)
	params := workerExecutionParameters{
		TaskList:     taskList,
		Identity:     wOptions.Identity,
		MetricsScope: wOptions.MetricsScope,
		Logger:       env.logger,
		UserContext:  wOptions.BackgroundActivityContext,
	}
	ensureRequiredParams(&params)

	var activities []activity
	for fnName, tasklistActivity := range env.taskListSpecificActivities {
		if _, ok := tasklistActivity.taskLists[taskList]; ok {
			ae := &activityExecutor{name: fnName, fn: tasklistActivity.fn}
			activities = append(activities, &activityExecutorWrapper{activityExecutor: ae, env: env})
		}
	}

	addActivities := func(registeredActivities []activity) {
		for _, a := range registeredActivities {
			fnName := a.ActivityType().Name
			if _, ok := env.taskListSpecificActivities[fnName]; ok {
				// activity is registered to a specific taskList, so ignore it from the global registered activities.
				continue
			}
			activities = append(activities, &activityExecutorWrapper{activityExecutor: a.(*activityExecutor), env: env})
		}
	}

	addActivities(env.testSuite.hostEnv.getRegisteredActivities())
	addActivities(getHostEnvironment().getRegisteredActivities())

	if len(activities) == 0 {
		panic(fmt.Sprintf("no activity is registered for tasklist '%v'", taskList))
	}

	taskHandler := newActivityTaskHandler(activities, env.service, params)
	return taskHandler
}

func newTestActivityTask(workflowID, runID, activityID, activityType string, input []byte) *shared.PollForActivityTaskResponse {
	task := &shared.PollForActivityTaskResponse{
		WorkflowExecution: &shared.WorkflowExecution{
			WorkflowId: common.StringPtr(workflowID),
			RunId:      common.StringPtr(runID),
		},
		ActivityId:                    common.StringPtr(activityID),
		TaskToken:                     []byte(activityID), // use activityID as TaskToken so we can map TaskToken in heartbeat calls.
		ActivityType:                  &shared.ActivityType{Name: common.StringPtr(activityType)},
		Input:                         input,
		ScheduledTimestamp:            common.Int64Ptr(time.Now().UnixNano()),
		ScheduleToCloseTimeoutSeconds: common.Int32Ptr(60),
		StartedTimestamp:              common.Int64Ptr(time.Now().UnixNano()),
		StartToCloseTimeoutSeconds:    common.Int32Ptr(60),
	}
	return task
}

func (env *testWorkflowEnvironmentImpl) NewTimer(d time.Duration, callback resultHandler) *timerInfo {
	nextID := env.nextID()
	timerInfo := &timerInfo{getStringID(nextID)}
	timer := env.mockClock.AfterFunc(d, func() {
		env.postCallback(func() {
			delete(env.timers, timerInfo.timerID)
			callback(nil, nil)
			if env.onTimerFiredListener != nil {
				env.onTimerFiredListener(timerInfo.timerID)
			}
		}, true)
	})
	env.timers[timerInfo.timerID] = &testTimerHandle{
		callback:       callback,
		timer:          timer,
		mockTimeToFire: env.mockClock.Now().Add(d),
		wallTimeToFire: env.wallClock.Now().Add(d),
		duration:       d,
		timerID:        nextID,
	}
	if env.onTimerScheduledListener != nil {
		env.onTimerScheduledListener(timerInfo.timerID, d)
	}
	return timerInfo
}

func (env *testWorkflowEnvironmentImpl) Now() time.Time {
	return env.mockClock.Now()
}

func (env *testWorkflowEnvironmentImpl) WorkflowInfo() *WorkflowInfo {
	return env.workflowInfo
}

func (env *testWorkflowEnvironmentImpl) RegisterCancelHandler(handler func()) {
	env.workflowCancelHandler = handler
}

func (env *testWorkflowEnvironmentImpl) RegisterSignalHandler(handler func(name string, input []byte)) {
	env.signalHandler = handler
}

func (env *testWorkflowEnvironmentImpl) RequestCancelWorkflow(domainName, workflowID, runID string) error {
	if env.workflowInfo.WorkflowExecution.ID == workflowID {
		// cancel current workflow
		env.workflowCancelHandler()

		// check if current workflow is a child workflow
		parentEnv := env.parentEnv
		if parentEnv != nil && parentEnv.onChildWorkflowCanceledListener != nil {
			parentEnv.postCallback(func() {
				parentEnv.onChildWorkflowCanceledListener(env.workflowInfo)
			}, false)
		}
	} else if childHandle, ok := env.childWorkflows[workflowID]; ok {
		// current workflow is a parent workflow, and we are canceling a child workflow
		delete(env.childWorkflows, workflowID)
		childEnv := childHandle.env
		childEnv.cancelWorkflow()
	}
	return nil
}

func (env *testWorkflowEnvironmentImpl) ExecuteChildWorkflow(options workflowOptions, callback resultHandler, startedHandler func(r WorkflowExecution, e error)) error {
	childEnv := env.newTestWorkflowEnvironmentForChild(&options)
	env.logger.Sugar().Infof("ExecuteChildWorkflow: %v", options.workflowType.Name)

	// start immediately
	env.runningCount.Inc()
	startedHandler(childEnv.workflowInfo.WorkflowExecution, nil)

	go func() {
		childEnv.executeWorkflowInternal(options.workflowType.Name, options.input)
		env.postCallback(func() {
			delete(env.childWorkflows, options.workflowID)
			// deliver result
			callback(childEnv.testResult, childEnv.testError)

			if env.onChildWorkflowCompletedListener != nil {
				env.onChildWorkflowCompletedListener(childEnv.workflowInfo, childEnv.testResult, childEnv.testError)
			}
		}, true)
		env.runningCount.Dec()
	}()

	return nil
}

func (env *testWorkflowEnvironmentImpl) SideEffect(f func() ([]byte, error), callback resultHandler) {
	callback(f())
}

func (env *testWorkflowEnvironmentImpl) nextID() int {
	activityID := env.counterID
	env.counterID++
	return activityID
}

func (env *testWorkflowEnvironmentImpl) onPanicHandler(f func(err error)) {
	env.panicHandler = f
}

func getStringID(intID int) string {
	return fmt.Sprintf("%d", intID)
}

func (env *testWorkflowEnvironmentImpl) getActivityInfo(activityID, activityType string) *ActivityInfo {
	return &ActivityInfo{
		ActivityID:        activityID,
		ActivityType:      ActivityType{Name: activityType},
		TaskToken:         []byte(activityID),
		WorkflowExecution: env.workflowInfo.WorkflowExecution,
	}
}

func (env *testWorkflowEnvironmentImpl) cancelWorkflow() {
	env.postCallback(func() {
		// RequestCancelWorkflow needs to be run in main thread
		env.RequestCancelWorkflow(env.workflowInfo.Domain,
			env.workflowInfo.WorkflowExecution.ID,
			env.workflowInfo.WorkflowExecution.RunID)
	}, true)
}

func (env *testWorkflowEnvironmentImpl) signalWorkflow(name string, input interface{}) {
	data, err := env.testSuite.hostEnv.encodeArg(input)
	if err != nil {
		panic(err)
	}
	env.postCallback(func() {
		env.signalHandler(name, data)
	}, true)
}

// make sure interface is implemented
var _ workflowEnvironment = (*testWorkflowEnvironmentImpl)(nil)
