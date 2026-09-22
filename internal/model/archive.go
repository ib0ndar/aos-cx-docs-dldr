package model

const (
	PDFOriginDirectMapped     = "direct-mapped"
	PDFOriginSelectedRoute    = "selected-route-response"
	PDFOriginSourceAdvertised = "source-advertised-original"
	PDFOriginHPEExportAll     = "hpe-whole-document-export"
)

type NoticeKind string

const (
	NoticeExternalLinkRetained   NoticeKind = "external-link-retained"
	NoticeNavigationStyleOmitted NoticeKind = "navigation-style-omitted"
	NoticeCSSCleanup             NoticeKind = "css-cleanup"
	NoticeSourcePolicy           NoticeKind = "source-policy"
)

type Notice struct {
	Kind    NoticeKind `json:"kind"`
	Message string     `json:"message"`
}

type StylesheetRecovery struct {
	Kind                    string                      `json:"kind,omitempty"`
	BrokenURL               string                      `json:"broken_url"`
	HTTPStatus              int                         `json:"http_status"`
	BrokenDeclarations      []BrokenStylesheetReference `json:"broken_declarations,omitempty"`
	ReplacementURL          string                      `json:"replacement_url"`
	ReplacementFinalURL     string                      `json:"replacement_final_url"`
	ReplacementSourceSHA256 string                      `json:"replacement_source_sha256"`
	ReplacementSourceSize   int64                       `json:"replacement_source_size"`
	TableStyleFamilies      []string                    `json:"table_style_families"`
	AffectedTopics          []string                    `json:"affected_topics"`
}

type BrokenStylesheetReference struct {
	URL        string `json:"url"`
	HTTPStatus int    `json:"http_status"`
}

type PDFAvailability struct {
	URL       string `json:"url,omitempty"`
	FinalURL  string `json:"final_url,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Checked   bool   `json:"checked"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type SourceAvailability struct {
	URL       string `json:"url"`
	ProbeURL  string `json:"probe_url,omitempty"`
	FinalURL  string `json:"final_url,omitempty"`
	Format    string `json:"format"`
	Checked   bool   `json:"checked"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type SkippedUnavailableGuide struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Kind             string `json:"kind"`
	MappedSourceURL  string `json:"mapped_source_url"`
	ProbeURL         string `json:"probe_url,omitempty"`
	FinalURL         string `json:"final_url,omitempty"`
	Checked          bool   `json:"checked"`
	Available        bool   `json:"available"`
	Reason           string `json:"reason"`
	RetainedPrevious bool   `json:"retained_previous"`
}

type DocumentPlan struct {
	Document              Document                `json:"document"`
	PDFURL                string                  `json:"pdf_url,omitempty"`
	PDFVerified           bool                    `json:"pdf_verified,omitempty"`
	PDFOrigin             string                  `json:"pdf_origin,omitempty"`
	PDFPreferred          bool                    `json:"pdf_preferred,omitempty"`
	Topics                []Topic                 `json:"topics"`
	TOC                   []TocEntry              `json:"toc"`
	Styles                []string                `json:"styles"`
	Notices               []Notice                `json:"notices"`
	Warnings              []string                `json:"warnings"`
	Inputs                []SourceInput           `json:"inputs"`
	UnavailableNavigation []UnavailableNavigation `json:"unavailable_navigation,omitempty"`
	Inventory             *InventoryEvidence      `json:"inventory,omitempty"`
}

type Topic struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	FetchURL string `json:"fetch_url,omitempty"`
}

type TocEntry struct {
	Title    string     `json:"title"`
	URL      string     `json:"url,omitempty"`
	Children []TocEntry `json:"children,omitempty"`
}

type SourceInput struct {
	RequestedURL string `json:"requested_url"`
	FinalURL     string `json:"final_url"`
	Role         string `json:"role"`
	ContentType  string `json:"content_type"`
	SHA256       string `json:"source_sha256"`
	Size         int    `json:"size"`
	ObservedAt   string `json:"observed_at"`
}

type UnavailableNavigation struct {
	URL        string `json:"url"`
	HTTPStatus int    `json:"http_status"`
	Role       string `json:"role"`
}

type MissingResource struct {
	RequestedURL        string `json:"requested_url"`
	FinalURL            string `json:"final_url,omitempty"`
	ReferringTopicURL   string `json:"referring_topic_url"`
	GeneratedPagePath   string `json:"generated_page_path"`
	GeneratedPageSHA256 string `json:"generated_page_sha256"`
	FailureClass        string `json:"failure_class"`
	HTTPStatus          int    `json:"http_status,omitempty"`
	RetrievalStage      string `json:"retrieval_stage,omitempty"`
	ApplicationAttempts int    `json:"application_attempts,omitempty"`
	ApplicationRetries  int    `json:"application_retries,omitempty"`
	Message             string `json:"message"`
	ElementType         string `json:"element_type"`
	Alt                 string `json:"alt,omitempty"`
	Title               string `json:"title,omitempty"`
	Caption             string `json:"caption,omitempty"`
	DeclaredWidth       int    `json:"declared_width,omitempty"`
	DeclaredHeight      int    `json:"declared_height,omitempty"`
	ExpectedRasterType  string `json:"expected_raster_type"`
	PlaceholderID       string `json:"placeholder_id"`
	ObservedAt          string `json:"observed_at"`
}

func ImagePlaceholderCount(resources []MissingResource) int {
	ids := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if resource.PlaceholderID != "" {
			ids[resource.PlaceholderID] = struct{}{}
		}
	}
	return len(ids)
}

type InventoryEvidence struct {
	Complete     bool     `json:"complete"`
	Kind         string   `json:"kind"`
	RootURL      string   `json:"root_url"`
	TOCURL       string   `json:"toc_url"`
	ChunkURLs    []string `json:"chunk_urls,omitempty"`
	Entries      int      `json:"navigation_entries"`
	UniqueTopics int      `json:"unique_topics"`
}

type PDFRecord struct {
	URL            string        `json:"url"`
	FinalURL       string        `json:"final_url"`
	Path           string        `json:"path"`
	Status         string        `json:"status"`
	MIME           string        `json:"content_type"`
	Size           int64         `json:"size"`
	SourceSHA256   string        `json:"source_sha256"`
	SHA256         string        `json:"sha256"`
	RetrievedAt    string        `json:"retrieved_at"`
	Provenance     string        `json:"provenance"`
	Origin         string        `json:"origin"`
	SourceVerified bool          `json:"source_verified,omitempty"`
	Inputs         []SourceInput `json:"source_inputs,omitempty"`
}

type GeneratedPDFSettings struct {
	SchemaVersion            int     `json:"schema_version"`
	OutlinePlanSchemaVersion int     `json:"outline_plan_schema_version"`
	FooterPlanSchemaVersion  int     `json:"footer_plan_schema_version"`
	HeadingStyleVersion      int     `json:"heading_style_version"`
	FooterStyleVersion       int     `json:"footer_style_version"`
	Paper                    string  `json:"paper"`
	MarginTopInches          float64 `json:"margin_top_inches"`
	MarginRightInches        float64 `json:"margin_right_inches"`
	MarginBottomInches       float64 `json:"margin_bottom_inches"`
	MarginLeftInches         float64 `json:"margin_left_inches"`
	PageNumberFormat         string  `json:"page_number_format"`
	PageNumberPosition       string  `json:"page_number_position"`
	FooterCategoryPolicy     string  `json:"footer_category_policy"`
	FirstVisiblePageNumber   int     `json:"first_visible_page_number"`
	CoverFooterVisible       bool    `json:"cover_footer_visible"`
	PrintBackground          bool    `json:"print_background"`
	PreferCSSPageSize        bool    `json:"prefer_css_page_size"`
	DisplayHeaderFooter      bool    `json:"display_header_footer"`
	GenerateTaggedPDF        bool    `json:"generate_tagged_pdf"`
	GenerateDocumentOutline  bool    `json:"generate_document_outline"`
}

type GeneratedPDFValidation struct {
	PageCount                  int      `json:"page_count"`
	LetterMediaBoxes           int      `json:"letter_media_boxes"`
	FontObjects                int      `json:"font_objects"`
	ImageObjects               int      `json:"image_objects"`
	AnnotationObjects          int      `json:"annotation_objects"`
	OutlineObjects             int      `json:"outline_objects"`
	TOCEntries                 int      `json:"toc_entries"`
	TOCLinksChecked            int      `json:"toc_links_checked"`
	LocalRequests              int      `json:"local_requests"`
	InlineDataRequests         int      `json:"inline_data_requests"`
	ScriptElements             int      `json:"script_elements"`
	LoadedImages               int      `json:"loaded_images"`
	ImagePlaceholders          int      `json:"image_placeholders"`
	ImagePlaceholderOverflows  int      `json:"image_placeholder_overflows"`
	InternalLinksChecked       int      `json:"internal_links_checked"`
	FailedImages               int      `json:"failed_images"`
	FailedImageDetails         []string `json:"failed_image_details,omitempty"`
	MissingTargets             int      `json:"missing_internal_targets"`
	ExternalRequests           int      `json:"external_requests"`
	CanonicalCoverTitles       int      `json:"canonical_cover_titles"`
	PublisherChromeElements    int      `json:"publisher_chrome_elements"`
	FixedOrStickyElements      int      `json:"fixed_or_sticky_elements"`
	OutlineEntries             int      `json:"outline_entries"`
	OutlineMaxDepth            int      `json:"outline_max_depth"`
	OutlineExternalURIs        int      `json:"outline_external_uris"`
	OutlineRepeatedTargets     int      `json:"outline_repeated_targets"`
	OutlineSupplementaryGroups int      `json:"outline_supplementary_groups"`
	FontsLoaded                bool     `json:"fonts_loaded"`
	ScriptExecutionDisabled    bool     `json:"script_execution_disabled"`
	ResourceClosureValidated   bool     `json:"resource_closure_validated"`
	CoverTypographyValidated   bool     `json:"cover_typography_validated"`
	HeadingTypographyValidated bool     `json:"heading_typography_validated"`
	HeadingPaginationValidated bool     `json:"heading_pagination_validated"`
	FooterOverlayValidated     bool     `json:"footer_overlay_validated"`
	OutlineValidated           bool     `json:"outline_validated"`
	StructuralValidated        bool     `json:"structural_validated"`
}

type GeneratedPDFRecord struct {
	Path                              string                  `json:"path"`
	Status                            string                  `json:"status"`
	CreatedAt                         string                  `json:"created_at"`
	Error                             string                  `json:"error,omitempty"`
	Renderer                          string                  `json:"renderer,omitempty"`
	RendererVersion                   string                  `json:"renderer_version,omitempty"`
	RendererRevision                  string                  `json:"renderer_revision,omitempty"`
	ExecutableSHA256                  string                  `json:"renderer_executable_sha256,omitempty"`
	Postprocessor                     string                  `json:"postprocessor,omitempty"`
	PostprocessorVersion              string                  `json:"postprocessor_version,omitempty"`
	PostprocessorExecutableSHA256     string                  `json:"postprocessor_executable_sha256,omitempty"`
	PostprocessorBundleSHA256         string                  `json:"postprocessor_bundle_sha256,omitempty"`
	OutlinePlanSHA256                 string                  `json:"outline_plan_sha256,omitempty"`
	FooterPlanSHA256                  string                  `json:"footer_plan_sha256,omitempty"`
	FooterOverlayInputSHA256          string                  `json:"footer_overlay_input_sha256,omitempty"`
	FooterOverlayRenderer             string                  `json:"footer_overlay_renderer,omitempty"`
	FooterOverlayRendererVersion      string                  `json:"footer_overlay_renderer_version,omitempty"`
	FooterOverlayExecutableSHA256     string                  `json:"footer_overlay_executable_sha256,omitempty"`
	FooterEntries                     int                     `json:"footer_entries,omitempty"`
	SourceManifestSHA256              string                  `json:"source_manifest_sha256,omitempty"`
	InputSHA256                       string                  `json:"transient_input_sha256,omitempty"`
	SHA256                            string                  `json:"sha256,omitempty"`
	Size                              int64                   `json:"size,omitempty"`
	DurationMilliseconds              int64                   `json:"duration_milliseconds,omitempty"`
	PeakRSSBytes                      int64                   `json:"peak_rss_bytes,omitempty"`
	PostprocessDurationMilliseconds   int64                   `json:"postprocess_duration_milliseconds,omitempty"`
	PostprocessPeakRSSBytes           int64                   `json:"postprocess_peak_rss_bytes,omitempty"`
	FooterOverlayDurationMilliseconds int64                   `json:"footer_overlay_duration_milliseconds,omitempty"`
	FooterOverlayPeakRSSBytes         int64                   `json:"footer_overlay_peak_rss_bytes,omitempty"`
	Provenance                        string                  `json:"provenance,omitempty"`
	InputStatus                       string                  `json:"input_status,omitempty"`
	ImagePlaceholders                 int                     `json:"image_placeholders,omitempty"`
	Settings                          *GeneratedPDFSettings   `json:"settings,omitempty"`
	Validation                        *GeneratedPDFValidation `json:"validation,omitempty"`
}

type ArchiveResult struct {
	Document             Document             `json:"document"`
	OutputDir            string               `json:"-"`
	Status               string               `json:"status"`
	Format               string               `json:"format"`
	PDF                  *PDFRecord           `json:"pdf,omitempty"`
	HTML                 *HTMLArchive         `json:"html,omitempty"`
	Errors               []string             `json:"errors"`
	Notices              []Notice             `json:"notices"`
	Warnings             []string             `json:"warnings"`
	StylesheetRecoveries []StylesheetRecovery `json:"stylesheet_recoveries"`
	MissingResources     []MissingResource    `json:"missing_resources,omitempty"`
}

type FileRecord struct {
	URL                     string            `json:"url"`
	FetchURL                string            `json:"fetch_url,omitempty"`
	FinalURL                string            `json:"final_url,omitempty"`
	Path                    string            `json:"path"`
	Status                  string            `json:"status"`
	ContentType             string            `json:"content_type,omitempty"`
	Size                    int64             `json:"size,omitempty"`
	SourceSize              int64             `json:"source_size,omitempty"`
	SourceSHA256            string            `json:"source_sha256,omitempty"`
	SHA256                  string            `json:"sha256,omitempty"`
	Error                   string            `json:"error,omitempty"`
	Supplementary           bool              `json:"supplementary,omitempty"`
	SourceBookmarks         []string          `json:"source_bookmarks,omitempty"`
	SourceBookmarksComplete bool              `json:"source_bookmarks_complete,omitempty"`
	Metadata                map[string]string `json:"metadata,omitempty"`
	Copyright               []string          `json:"copyright,omitempty"`
}

type HTMLIntegrity struct {
	IndexSHA256       string `json:"index_sha256"`
	TOCSHA256         string `json:"toc_sha256"`
	SearchSHA256      string `json:"search_sha256"`
	SearchIndexSHA256 string `json:"search_index_sha256"`
	SearchJSSHA256    string `json:"search_js_sha256"`
	CheckedLinks      int    `json:"checked_local_links"`
}

type HTMLArchive struct {
	SchemaVersion        int                  `json:"schema_version"`
	Status               string               `json:"status"`
	SourceURL            string               `json:"source_url"`
	Inputs               []SourceInput        `json:"source_inputs"`
	Inventory            *InventoryEvidence   `json:"inventory"`
	Topics               []FileRecord         `json:"topics"`
	Assets               []FileRecord         `json:"assets"`
	Notices              []Notice             `json:"notices"`
	Warnings             []string             `json:"warnings"`
	Errors               []string             `json:"errors"`
	StylesheetRecoveries []StylesheetRecovery `json:"stylesheet_recoveries"`
	MissingResources     []MissingResource    `json:"missing_resources,omitempty"`
	Integrity            HTMLIntegrity        `json:"integrity"`
	GeneratedPDF         *GeneratedPDFRecord  `json:"generated_pdf,omitempty"`
	GeneratedAt          string               `json:"generated_at"`
}
