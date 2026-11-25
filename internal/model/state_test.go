package model

import (
	"errors"
	"testing"
)

func TestStateTransitionsLegal(t *testing.T) {
	cases := []struct{ from, to State }{
		{StateRunning, StateStopped},
		{StateRunning, StateDetached},
		{StateRunning, StateDead},
		{StateStopped, StateRunning},
		{StateStopped, StateDetached},
		{StateStopped, StateDead},
		{StateDetached, StateDead},
	}
	for _, c := range cases {
		if !c.from.CanTransitionTo(c.to) {
			t.Fatalf("%s -> %s 应合法", c.from, c.to)
		}
	}
}

func TestStateTransitionsIllegal(t *testing.T) {
	cases := []struct{ from, to State }{
		{StateDead, StateRunning},
		{StateDead, StateStopped},
		{StateDetached, StateStopped},
		{StateDetached, StateRunning},
		{StateRunning, StateRunning},
		{StateStopped, StateStopped},
	}
	for _, c := range cases {
		if c.from.CanTransitionTo(c.to) {
			t.Fatalf("%s -> %s 应非法", c.from, c.to)
		}
	}
	if len(StateDead.AllowedTargets()) != 0 {
		t.Fatal("死状态应为终态")
	}
}

func TestMachineTransition(t *testing.T) {
	m := NewMachine(StateRunning)
	if err := m.Transition(StateStopped); err != nil {
		t.Fatalf("合法转换失败: %v", err)
	}
	if m.State() != StateStopped {
		t.Fatalf("状态未更新: %s", m.State())
	}
	err := m.Transition(StateDetached)
	if err != nil {
		t.Fatalf("stopped -> detached 应合法: %v", err)
	}
	err = m.Transition(StateRunning)
	if err == nil {
		t.Fatal("detached -> running 应非法")
	}
	var te *TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("错误类型不符: %T", err)
	}
	if CodeOf(err) != CodeStateInvalidTransition {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
}

func TestMachineMustTransition(t *testing.T) {
	m := NewMachine(StateRunning)
	if err := m.MustTransition(StateStopped, StateRunning); err == nil {
		t.Fatal("起点不匹配时应失败")
	} else if CodeOf(err) != CodeStateInvalidTransition {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
	if err := m.MustTransition(StateRunning, StateStopped); err != nil {
		t.Fatalf("起点匹配时应成功: %v", err)
	}
	if m.Seq() != 1 {
		t.Fatalf("转换计数不符: %d", m.Seq())
	}
}
