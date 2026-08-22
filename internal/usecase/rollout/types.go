package rollout

import "time"

// Schema versions of the TRD 09 rollout contract: db upgrade accepts sources
// in [MinSourceVersion, MaxSourceVersion] and drives them to TargetVersion.
const (
	TargetVersion    = 41
	MinSourceVersion = 33
	MaxSourceVersion = 40
)

// Durable runtime_state keys owned by the rollout. The quarantine marker is
// written only by Create's adoption transaction; its read belongs to
// checkpoint 5.
const (
	KeyBaseline            = "v41_upgrade_baseline"
	KeyCutoff              = "v41_rollout_cutoff_unix_nanos"
	KeyPostflightStatus    = "v41_postflight_status"
	KeyPostflightDetail    = "v41_postflight_detail"
	KeyBackupPath          = "v41_upgrade_backup_path"
	KeyBackupBytes         = "v41_upgrade_backup_bytes"
	KeyBackupSHA256        = "v41_upgrade_backup_sha256"
	KeyBackupSourceVersion = "v41_upgrade_backup_source_version"
	KeyBackupVerifiedAt    = "v41_upgrade_backup_verified_at"
	KeyBackupNotRequiredAt = "v41_upgrade_backup_not_required_at"
	KeyLegacyQuarantineAt  = "legacy_identity_quarantine_applied_at"
)

// UpgradeOptions carries the operator inputs both backend methods receive.
// --yes is a CLI-only prompt-skip flag and never reaches this layer.
type UpgradeOptions struct {
	BackupDir string
}

// UpgradeKind is the operator-facing label of a Recovery Table row. Rows 1
// and 2 share one label; UpgradeKindForRow performs that single collapse.
type UpgradeKind int

const (
	UpgradeFreshUpgrade UpgradeKind = iota
	UpgradeAdoption
	UpgradeResumeNeeded
	UpgradeAlreadyComplete
)

// RolloutRow is the pre-collapse Recovery Table classification. Row 1 and
// row 2 stay distinct so a resumed run proves it revalidated a backup rather
// than skipping straight to AlreadyComplete.
type RolloutRow int

const (
	RolloutRowFreshCapture RolloutRow = iota
	RolloutRowFreshResume
	RolloutRowAdoption
	RolloutRowResumeNeeded
	RolloutRowAlreadyComplete
)

// UpgradeKindForRow collapses rows 1 and 2 onto the shared FreshUpgrade
// label. It is a total mapping, never a second classifier.
func UpgradeKindForRow(row RolloutRow) UpgradeKind {
	switch row {
	case RolloutRowFreshCapture, RolloutRowFreshResume:
		return UpgradeFreshUpgrade
	case RolloutRowAdoption:
		return UpgradeAdoption
	case RolloutRowResumeNeeded:
		return UpgradeResumeNeeded
	default:
		return UpgradeAlreadyComplete
	}
}

// UpgradePreview is the read-only classification result the CLI renders
// before prompting. ResolvedBackupDir stays empty for kinds that never
// create a backup; DatabasePath lets the CLI render the frozen confirmation
// text naming the configured database.
type UpgradePreview struct {
	Kind              UpgradeKind
	FromVersion       int
	ToVersion         int
	ResolvedBackupDir string
	DatabasePath      string
}

// UpgradeReport is the outcome of one Apply invocation. RolloutAdvanced
// answers "did this call durably write rollout state", never "did the
// schema version change".
type UpgradeReport struct {
	Kind                              UpgradeKind
	RolloutAdvanced                   bool
	FromVersion                       int
	ToVersion                         int
	Backup                            BackupIdentity
	PostflightOK                      bool
	PostflightDetail                  string
	BaselineJobsWithoutIdentity       int
	BaselineActivationsWithoutContent int
	PostJobsWithoutIdentity           int
	PostActivationsWithoutContent     int
}

// BackupIdentity mirrors either the five durable v41_upgrade_backup_* keys
// or the single mutually exclusive not-required marker.
type BackupIdentity struct {
	Path          string
	Bytes         int64
	SHA256        string
	SourceVersion int
	VerifiedAt    time.Time
	NotRequired   bool
	NotRequiredAt time.Time
}

// BackupIdentityShape is the three-shape read of the six backup-related
// keys. Any other reading is Corrupt, never a fourth shape.
type BackupIdentityShape int

const (
	BackupIdentityAbsent BackupIdentityShape = iota
	BackupIdentityPresent
	BackupIdentityNotRequired
)

// IdentityBaseline carries the two carve-out counters postflight compares
// against the durable baseline.
type IdentityBaseline struct {
	JobsCompletedWithoutResultIdentity int
	ActivationsWithoutContent          int
}

// PostflightStatus is the only durable postflight status vocabulary.
type PostflightStatus string

const (
	PostflightPassed PostflightStatus = "passed"
	PostflightFailed PostflightStatus = "failed"
)

// RolloutState separates presence, format validity, and parsed value for
// every durable key the classifier reads. Presence is never inferred from a
// value; validity is meaningful only when presence holds.
type RolloutState struct {
	BaselinePresent bool
	BaselineValid   bool
	Baseline        IdentityBaseline

	CutoffPresent   bool
	CutoffValid     bool
	CutoffUnixNanos int64

	BackupPathPresent bool
	BackupPathValid   bool
	BackupPath        string

	BackupBytesPresent bool
	BackupBytesValid   bool
	BackupBytes        int64

	BackupSHA256Present bool
	BackupSHA256Valid   bool
	BackupSHA256        string

	BackupSourceVersionPresent bool
	BackupSourceVersionValid   bool
	BackupSourceVersion        int

	BackupVerifiedAtPresent bool
	BackupVerifiedAtValid   bool
	BackupVerifiedAt        time.Time

	BackupNotRequiredAtPresent bool
	BackupNotRequiredAtValid   bool
	BackupNotRequiredAt        time.Time

	PostflightPresent bool
	PostflightValid   bool
	PostflightStatus  PostflightStatus

	PostflightDetailPresent bool
	PostflightDetail        string
}

// SummaryDiscoveryDrainStatus is the content-free result of the TRD 08
// rollback drain precondition query.
type SummaryDiscoveryDrainStatus struct {
	Clear                    bool
	PendingSessionIdentities []string
}
