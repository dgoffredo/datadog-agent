// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build functionaltests
// +build functionaltests

package tests

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/security/secl/rules"
	"github.com/stretchr/testify/assert"
)

func TestContainerCreatedAt(t *testing.T) {
	ruleDefs := []*rules.RuleDefinition{
		{
			ID:         "test_container_created_at",
			Expression: `container.id != "" && container.created_at < 3s && open.file.path == "{{.Root}}/test-open"`,
		},
		{
			ID:         "test_container_created_at_delay",
			Expression: `container.id != "" && container.created_at > 3s && open.file.path == "{{.Root}}/test-open-delay"`,
		},
	}
	test, err := newTestModule(t, nil, ruleDefs, testOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer test.Close()

	testFile, _, err := test.Path("test-open")
	if err != nil {
		t.Fatal(err)
	}

	testFileDelay, _, err := test.Path("test-open-delay")
	if err != nil {
		t.Fatal(err)
	}

	dockerWrapper, err := newDockerCmdWrapper(test.Root(), test.Root(), "ubuntu")
	if err != nil {
		t.Skip("Skipping created time in containers tests: Docker not available")
		return
	}
	defer dockerWrapper.stop()

	dockerWrapper.Run(t, "container-created-at", func(t *testing.T, kind wrapperType, cmdFunc func(cmd string, args []string, envs []string) *exec.Cmd) {
		test.WaitSignal(t, func() error {
			cmd := cmdFunc("touch", []string{testFile}, nil)
			return cmd.Run()
		}, func(event *model.Event, rule *rules.Rule) {
			assertTriggeredRule(t, rule, "test_container_created_at")
			assertFieldEqual(t, event, "open.file.path", testFile)
			assertFieldNotEmpty(t, event, "container.id", "container id shouldn't be empty")

			test.validateOpenSchema(t, event)
		})
	})

	dockerWrapper.Run(t, "container-created-at-delay", func(t *testing.T, kind wrapperType, cmdFunc func(cmd string, args []string, envs []string) *exec.Cmd) {
		test.WaitSignal(t, func() error {
			cmd := cmdFunc("touch", []string{testFileDelay}, nil) // shouldn't trigger an event
			if err := cmd.Run(); err != nil {
				return err
			}
			time.Sleep(3 * time.Second)
			cmd = cmdFunc("touch", []string{testFileDelay}, nil)
			return cmd.Run()
		}, func(event *model.Event, rule *rules.Rule) {
			assertTriggeredRule(t, rule, "test_container_created_at_delay")
			assertFieldEqual(t, event, "open.file.path", testFileDelay)
			assertFieldNotEmpty(t, event, "container.id", "container id shouldn't be empty")
			createdAtNano, _ := event.GetFieldValue("container.created_at")
			createdAt := time.Unix(0, int64(createdAtNano.(int)))
			assert.True(t, time.Now().Sub(createdAt) > 3*time.Second)

			test.validateOpenSchema(t, event)
		})
	})
}

func TestContainerScopedVariable(t *testing.T) {
	ruleDefs := []*rules.RuleDefinition{
		{
			ID:         "test_container_set_scoped_variable",
			Expression: `open.file.path == "/tmp/test-open"`,
			Actions: []rules.ActionDefinition{{
				Set: &rules.SetDefinition{
					Name:  "var1",
					Value: true,
					Scope: "container",
				},
			}},
		}, {
			ID:         "test_container_check_scoped_variable",
			Expression: `open.file.path == "/tmp/test-open-2" && ${container.var1} == true`,
		},
	}

	test, err := newTestModule(t, nil, ruleDefs, testOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer test.Close()

	wrapper, err := newDockerCmdWrapper(test.Root(), test.Root(), "alpine")
	if err != nil {
		t.Skip("docker no available")
		return
	}

	if _, err := wrapper.start(); err != nil {
		t.Fatal(err)
	}
	defer wrapper.stop()

	wrapper.RunTest(t, "set-var", func(t *testing.T, kind wrapperType, cmdFunc func(cmd string, args []string, envs []string) *exec.Cmd) {
		test.WaitSignal(t, func() error {
			cmd := cmdFunc("/bin/touch", []string{"/tmp/test-open"}, nil)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w", out, err)
			}
			return nil
		}, func(event *model.Event, rule *rules.Rule) {
			assert.Equal(t, "test_container_set_scoped_variable", rule.ID, "wrong rule triggered")
		})
	})

	wrapper.RunTest(t, "check-var", func(t *testing.T, kind wrapperType, cmdFunc func(cmd string, args []string, envs []string) *exec.Cmd) {
		test.WaitSignal(t, func() error {
			cmd := cmdFunc("/bin/touch", []string{"/tmp/test-open-2"}, nil)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w", out, err)
			}
			return nil
		}, func(event *model.Event, rule *rules.Rule) {
			assert.Equal(t, "test_container_check_scoped_variable", rule.ID, "wrong rule triggered")
		})
	})
}
