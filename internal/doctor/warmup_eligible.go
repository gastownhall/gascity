package doctor

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *AgentSessionsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BDSplitStoreCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BdBackupSizeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BeadsRoleCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BeadsStoreCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BinaryCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BuiltinPackFamilyCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *CityConfigCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *CityStructureCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ConfigRefsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ConfigSemanticsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ConfigValidCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ControllerCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *CustomTypesCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DeprecatedAttachmentFieldsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DoltConfigCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DoltNomsSizeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DoltServerCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DoltBackupCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DoltVersionCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DurationRangeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *EventLogSizeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *EventsLogCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ImplicitImportCacheCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *InstructionsFileCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *NestedWorktreePruneCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *OrderFiringCurrentCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *OrphanSessionsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *PackCacheCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *PreStartScriptsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *PostgresAuthCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ProviderParityCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *RigBeadsCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *RigDoltServerCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *RigGitCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *RigPackCoverageCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *RigPathCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *SkillCollisionCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *WorktreeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *WorktreeDiskSizeCheck) WarmupEligible() bool { return false }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *ZombieSessionsCheck) WarmupEligible() bool { return false }
