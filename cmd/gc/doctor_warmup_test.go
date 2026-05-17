package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestCommandDoctorChecksWarmupEligibleDefaultsFalse(t *testing.T) {
	checks := []doctor.Check{
		&codexHooksDriftCheck{},
		&doltDriftCheck{},
		&doltTopologyCheck{},
		&importStateDoctorCheck{},
		&jsonlArchiveDoctorCheck{},
		&mcpConfigDoctorCheck{},
		&mcpSharedTargetDoctorCheck{},
		&sessionModelDoctorCheck{},
		&v2RoutedToNamespaceCheck{},
		v2AgentFormatCheck{},
		v2DefaultRigImportFormatCheck{},
		v2ImportFormatCheck{},
		v2PromptTemplateSuffixCheck{},
		v2RigPathSiteBindingCheck{},
		v2ScriptsLayoutCheck{},
		v2WorkspaceNameCheck{},
	}

	for _, c := range checks {
		t.Run(fmt.Sprintf("%T", c), func(t *testing.T) {
			if c.WarmupEligible() {
				t.Errorf("%T.WarmupEligible() = true, want false", c)
			}
		})
	}
}
