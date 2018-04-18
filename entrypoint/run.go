/*
Copyright 2018 The Kubernetes Authors.

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

package entrypoint

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// InternalErrorCode is what we write to the marker file to
	// indicate that we failed to start the wrapped command
	InternalErrorCode = "127"

	// DefaultTimeout is the default timeout for the test
	// process before SIGINT is sent
	DefaultTimeout = 120 * time.Minute

	// DefaultGracePeriod is the default timeout for the test
	// process after SIGINT is sent before SIGKILL is sent
	DefaultGracePeriod = 15 * time.Second
)

var (
	// errTimedOut is used as the command's error when the command
	// is terminated after the timeout is reached
	errTimedOut = errors.New("process timed out")
)

// Run executes the process as configured, writing the output
// to the process log and the exit code to the marker file on
// exit.
func (o Options) Run() error {
	processLogFile, err := os.Create(o.ProcessLog)
	if err != nil {
		return fmt.Errorf("could not open output process logfile: %v", err)
	}
	output := io.MultiWriter(os.Stdout, processLogFile)
	logrus.SetOutput(output)

	executable := o.Args[0]
	var arguments []string
	if len(o.Args) > 1 {
		arguments = o.Args[1:]
	}
	command := exec.Command(executable, arguments...)
	command.Stderr = output
	command.Stdout = output
	if err := command.Start(); err != nil {
		if err := ioutil.WriteFile(o.MarkerFile, []byte(InternalErrorCode), os.ModePerm); err != nil {
			return fmt.Errorf("could not write to marker file: %v", err)
		}
		return fmt.Errorf("could not start the process: %v", err)
	}

	timeout := time.Duration(optionOrDefault(o.Timeout, DefaultTimeout))
	var commandErr error
	cancelled := false
	done := make(chan error)
	go func() {
		done <- command.Wait()
	}()
	select {
	case err := <-done:
		commandErr = err
	case <-time.After(timeout):
		logrus.Errorf("Process did not finish before %s timeout", timeout)
		cancelled = true
		if err := command.Process.Signal(os.Interrupt); err != nil {
			logrus.WithError(err).Error("Could not interrupt process after timeout")
		}
		gracePeriod := time.Duration(optionOrDefault(o.GracePeriod, DefaultGracePeriod))
		select {
		case <-done:
			logrus.Errorf("Process gracefully exited before %s grace period", gracePeriod)
			// but we ignore the output error as we will want errTimedOut
		case <-time.After(gracePeriod):
			logrus.Errorf("Process did not exit before %s grace period", gracePeriod)
			if err := command.Process.Kill(); err != nil {
				logrus.WithError(err).Error("Could not kill process after grace period")
			}
		}
	}

	var returnCode string
	if cancelled {
		returnCode = InternalErrorCode
		commandErr = errTimedOut
	} else {
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok {
			returnCode = strconv.Itoa(status.ExitStatus())
		} else if commandErr == nil {
			returnCode = "0"
		} else {
			returnCode = "1"
		}
	}

	if err := ioutil.WriteFile(o.MarkerFile, []byte(returnCode), os.ModePerm); err != nil {
		return fmt.Errorf("could not write return code to marker file: %v", err)
	}
	if commandErr != nil {
		return fmt.Errorf("wrapped process failed with code %s: %v", returnCode, err)
	}
	return nil
}

// optionOrDefault defaults to a value if option
// is the zero value
func optionOrDefault(option, defaultValue time.Duration) time.Duration {
	if option == 0 {
		return defaultValue
	}

	return option
}
