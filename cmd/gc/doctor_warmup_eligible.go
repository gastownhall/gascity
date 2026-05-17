package main

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *codexHooksDriftCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *doltDriftCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *doltTopologyCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *importStateDoctorCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *jsonlArchiveDoctorCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (*mcpConfigDoctorCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (*mcpSharedTargetDoctorCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *sessionModelDoctorCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *v2RoutedToNamespaceCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2AgentFormatCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2DefaultRigImportFormatCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2ImportFormatCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2PromptTemplateSuffixCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2RigPathSiteBindingCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2ScriptsLayoutCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (v2WorkspaceNameCheck) WarmupEligible() bool { return false }
