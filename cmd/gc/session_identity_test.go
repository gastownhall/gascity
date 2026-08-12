package main

import (
	"reflect"
	"testing"
)

func TestDesiredSessionIdentity(t *testing.T) {
	tests := []struct {
		name string
		in   sessionIdentityInputs
		want map[string]string
	}{
		{
			name: "adoption base omits agent_name and pool_slot",
			in: sessionIdentityInputs{
				SessionName:       "city-worker",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "tok-1",
			},
			want: map[string]string{
				"session_name":       "city-worker",
				"state":              "active",
				"generation":         "1",
				"continuation_epoch": "1",
				"instance_token":     "tok-1",
			},
		},
		{
			name: "create base carries agent_name, omits session_name",
			in: sessionIdentityInputs{
				AgentName:         "gastown/worker",
				State:             "start-pending",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "tok-2",
			},
			want: map[string]string{
				"agent_name":         "gastown/worker",
				"state":              "start-pending",
				"generation":         "1",
				"continuation_epoch": "1",
				"instance_token":     "tok-2",
			},
		},
		{
			name: "pool slot stamped when positive",
			in: sessionIdentityInputs{
				AgentName:         "gastown/worker-3",
				SessionName:       "city-worker-3",
				State:             "active",
				Generation:        2,
				ContinuationEpoch: 5,
				InstanceToken:     "tok-3",
				PoolSlot:          3,
			},
			want: map[string]string{
				"agent_name":         "gastown/worker-3",
				"session_name":       "city-worker-3",
				"state":              "active",
				"generation":         "2",
				"continuation_epoch": "5",
				"instance_token":     "tok-3",
				"pool_slot":          "3",
			},
		},
		{
			name: "zero pool slot omitted",
			in: sessionIdentityInputs{
				AgentName:         "a",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "t",
				PoolSlot:          0,
			},
			want: map[string]string{
				"agent_name":         "a",
				"state":              "active",
				"generation":         "1",
				"continuation_epoch": "1",
				"instance_token":     "t",
			},
		},
		{
			name: "config-resolved singleton stamps canonical name without slot",
			in: sessionIdentityInputs{
				AgentName:         "gastown/worker",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "t",
				ConfigResolved:    true,
			},
			want: map[string]string{
				"agent_name":              "gastown/worker",
				"state":                   "active",
				"generation":              "1",
				"continuation_epoch":      "1",
				"instance_token":          "t",
				"canonical_instance_name": "gastown/worker",
			},
		},
		{
			name: "config-resolved pool instance stamps canonical name and slot",
			in: sessionIdentityInputs{
				AgentName:         "gastown/worker-3",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "t",
				PoolSlot:          3,
				ConfigResolved:    true,
			},
			want: map[string]string{
				"agent_name":              "gastown/worker-3",
				"state":                   "active",
				"generation":              "1",
				"continuation_epoch":      "1",
				"instance_token":          "t",
				"pool_slot":               "3",
				"canonical_instance_name": "gastown/worker-3",
				"canonical_pool_slot":     "3",
			},
		},
		{
			name: "orphan (not config-resolved) mints no canonical record",
			in: sessionIdentityInputs{
				AgentName:         "some-session",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "t",
				PoolSlot:          2,
				ConfigResolved:    false,
			},
			want: map[string]string{
				"agent_name":         "some-session",
				"state":              "active",
				"generation":         "1",
				"continuation_epoch": "1",
				"instance_token":     "t",
				"pool_slot":          "2",
			},
		},
		{
			name: "config-resolved but empty agent name stamps no canonical record",
			in: sessionIdentityInputs{
				SessionName:       "city-worker",
				State:             "active",
				Generation:        1,
				ContinuationEpoch: 1,
				InstanceToken:     "t",
				ConfigResolved:    true,
			},
			want: map[string]string{
				"session_name":       "city-worker",
				"state":              "active",
				"generation":         "1",
				"continuation_epoch": "1",
				"instance_token":     "t",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := desiredSessionIdentity(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("desiredSessionIdentity() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
