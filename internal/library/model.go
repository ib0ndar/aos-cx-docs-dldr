package library

import "aos-cx-docs-dldr/internal/model"

const Application = "aos-cx-docs-dldr"
const (
	LibraryNativeSchema        = 1
	LegacyJournalNativeSchema  = 1
	UpgradeJournalNativeSchema = 2
	NativeSchema               = LibraryNativeSchema
)

type Attempt struct {
	ID                   string                     `json:"id"`
	Title                string                     `json:"title"`
	Status               string                     `json:"status"`
	Format               string                     `json:"format"`
	Errors               []string                   `json:"errors"`
	Notices              []model.Notice             `json:"notices"`
	Warnings             []string                   `json:"warnings"`
	StylesheetRecoveries []model.StylesheetRecovery `json:"stylesheet_recoveries"`
	MissingResources     []model.MissingResource    `json:"missing_resources,omitempty"`
	RetainedPrevious     bool                       `json:"retained_previous"`
}

type Guide struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Path             string `json:"path"`
	Format           string `json:"format"`
	Status           string `json:"status"`
	SourceURL        string `json:"source_url"`
	RouteOrigin      string `json:"route_origin,omitempty"`
	TopicCount       int    `json:"topic_count"`
	AssetCount       int    `json:"asset_count"`
	PlaceholderCount int    `json:"image_placeholder_count,omitempty"`
}

type Manifest struct {
	Application         string                          `json:"application"`
	ApplicationVersion  string                          `json:"application_version"`
	SchemaVersion       int                             `json:"schema_version"`
	NativeSchemaVersion int                             `json:"native_schema_version"`
	RunID               string                          `json:"run_id"`
	Platform            string                          `json:"platform"`
	Version             string                          `json:"version"`
	UpdatedAt           string                          `json:"updated_at"`
	Status              string                          `json:"status"`
	Guides              []Guide                         `json:"guides"`
	PDFGuides           map[string]model.ArchiveResult  `json:"pdf_guides"`
	HTMLGuides          map[string]model.ArchiveResult  `json:"html_guides,omitempty"`
	Attempts            []Attempt                       `json:"latest_attempts"`
	Errors              []string                        `json:"errors"`
	CatalogueURL        string                          `json:"catalogue_source_url"`
	CatalogueFetchedAt  string                          `json:"catalogue_fetched_at"`
	CatalogueWarnings   []string                        `json:"catalogue_warnings"`
	SkippedUnavailable  []model.SkippedUnavailableGuide `json:"skipped_unavailable_guides,omitempty"`
	UpgradedFromVersion string                          `json:"upgraded_from_application_version,omitempty"`
}

type Change struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

type History struct {
	At                         string                          `json:"at"`
	ApplicationVersion         string                          `json:"application_version"`
	PreviousSnapshot           string                          `json:"previous_snapshot,omitempty"`
	Changes                    map[string]Change               `json:"changes"`
	Attempts                   []Attempt                       `json:"attempts"`
	SkippedUnavailable         []model.SkippedUnavailableGuide `json:"skipped_unavailable_guides,omitempty"`
	UpgradedFromVersion        string                          `json:"upgraded_from_application_version,omitempty"`
	Operation                  string                          `json:"operation,omitempty"`
	PreviousApplicationVersion string                          `json:"previous_application_version,omitempty"`
	SnapshotDigest             string                          `json:"snapshot_digest,omitempty"`
}

type journal struct {
	Application            string `json:"application,omitempty"`
	NativeSchemaVersion    int    `json:"native_schema_version"`
	Operation              string `json:"operation,omitempty"`
	RunID                  string `json:"run_id"`
	PreviousRun            string `json:"previous_run"`
	Stage                  string `json:"stage"`
	PreviousSnapshot       string `json:"previous_snapshot"`
	FromApplicationVersion string `json:"from_application_version,omitempty"`
	ToApplicationVersion   string `json:"to_application_version,omitempty"`
	Platform               string `json:"platform,omitempty"`
	Version                string `json:"version,omitempty"`
	OwnerToken             string `json:"owner_token,omitempty"`
	SnapshotDigest         string `json:"snapshot_digest,omitempty"`
	StageDigest            string `json:"stage_digest,omitempty"`
}

type journalDiscriminator struct {
	NativeSchemaVersion int `json:"native_schema_version"`
}

type legacyPublicationJournal struct {
	Application         string `json:"application,omitempty"`
	NativeSchemaVersion int    `json:"native_schema_version"`
	RunID               string `json:"run_id"`
	PreviousRun         string `json:"previous_run"`
	Stage               string `json:"stage"`
	PreviousSnapshot    string `json:"previous_snapshot"`
}

type applicationUpgradeJournal struct {
	Application            string `json:"application"`
	NativeSchemaVersion    int    `json:"native_schema_version"`
	Operation              string `json:"operation"`
	RunID                  string `json:"run_id"`
	PreviousRun            string `json:"previous_run"`
	Stage                  string `json:"stage"`
	PreviousSnapshot       string `json:"previous_snapshot"`
	FromApplicationVersion string `json:"from_application_version"`
	ToApplicationVersion   string `json:"to_application_version"`
	Platform               string `json:"platform"`
	Version                string `json:"version"`
	OwnerToken             string `json:"owner_token"`
	SnapshotDigest         string `json:"snapshot_digest"`
	StageDigest            string `json:"stage_digest"`
}

type lockOwner struct {
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	StartedAt string `json:"started_at"`
	Token     string `json:"token"`
}
