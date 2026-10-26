package system

import (
	"context"
	"strings"
	"sync"
	"time"
	"wox/common"
	"wox/common/icons"
	"wox/plugin"
	notesplugin "wox/plugin/system/notes"
	"wox/plugin/system/quickjump"
	shellplugin "wox/plugin/system/shell"
	"wox/setting/definition"
	"wox/util"
	"wox/util/airdrop"
	"wox/util/clipboard"
	"wox/util/keyboard"
	"wox/util/selection"
	"wox/util/shell"

	"github.com/google/uuid"
)

var selectionIcon = icons.Get(icons.PluginSelection)

// selectionCommandPreview is the command name that, when used in a selection file
// query, causes the plugin to return only the file preview result instead of
// the full set of actions (copy path, open folder, preview, etc.).
const selectionCommandPreview = "preview"

const (
	enableSpaceQuickLookSettingKey = "enableSpaceQuickLook"
	selectionPluginID              = common.SelectionPluginID
	// quickLookInstanceName is the named secondary launcher session owned by the
	// Space Quick Look preview. It stays distinct from the selection hotkey
	// session so the two never replace each other's window.
	quickLookInstanceName = "selection-quicklook"
	// spaceQuickLookHoldDelay is the minimum Space hold before releasing closes
	// the preview (peek). It matches util/hotkey's holdModifierPressDelay so
	// hold-to-peek and hold-modifier hotkeys feel consistent.
	spaceQuickLookHoldDelay = 200 * time.Millisecond
	// quickLookOpsCapacity bounds the serialized open/close queue. Each Space
	// decision enqueues at most one operation, so human input never fills it.
	quickLookOpsCapacity = 16
)

func init() {
	plugin.AllSystemPlugin = append(plugin.AllSystemPlugin, &SelectionPlugin{})
}

type SelectionPlugin struct {
	api            plugin.API
	quickLookMu    sync.Mutex
	quickLookState *selectionSpaceQuickLookState
}

// selectionSpaceQuickLookState owns the platform monitor subscriptions and the
// key-state machine that drives macOS-like Space Quick Look behavior:
// short-press toggles the preview, holding Space for at least
// spaceQuickLookHoldDelay peeks (release closes).
type selectionSpaceQuickLookState struct {
	explorerSub quickjump.ExplorerRawKeySubscription
	dialogSub   quickjump.ExplorerRawKeySubscription
	// ops serializes open/close dispatches in keyboard decision order so a
	// later close can never overtake an earlier in-flight open of the same
	// named window (opens read the selection via the clipboard and are slow).
	ops chan func()
	now func() time.Time
	// openInstance dispatches an open and reports whether a window was actually
	// requested; closeInstance closes the named preview instance. Injectable
	// for tests.
	openInstance  func() bool
	closeInstance func()
	closed        bool

	mu             sync.Mutex
	spaceDown      bool
	spaceConsumed  bool
	invalidUntil   time.Time
	spacePressedAt time.Time
	// quickLookOpen is the desired state of the preview window: an open was
	// requested and no close decision has followed. It is set at decision time,
	// not completion time, so keyup can judge hold duration even while the
	// window is still opening.
	quickLookOpen bool
	// openInFlight marks an open that has been enqueued but not finished yet;
	// closeAfterOpen defers a close decision until that open completes.
	openInFlight   bool
	closeAfterOpen bool
}

func (i *SelectionPlugin) GetMetadata() plugin.Metadata {
	return plugin.Metadata{
		Id:            common.SelectionPluginID,
		Name:          "i18n:plugin_selection_plugin_name",
		Author:        "Wox Launcher",
		Website:       "https://github.com/Wox-launcher/Wox",
		Version:       "1.0.0",
		MinWoxVersion: "2.0.0",
		Runtime:       "Go",
		Description:   "i18n:plugin_selection_plugin_description",
		Icon:          selectionIcon.String(),
		Entry:         "",
		TriggerKeywords: []string{
			"*",
			"selection",
		},
		SupportedOS: []string{
			"Windows",
			"Macos",
			"Linux",
		},
		Commands: []plugin.MetadataCommand{
			{
				Command:     selectionCommandPreview,
				Description: "i18n:plugin_selection_command_preview",
			},
		},
		SettingDefinitions: definition.PluginSettingDefinitions{
			{
				Type: definition.PluginSettingDefinitionTypeCheckBox,
				Value: &definition.PluginSettingValueCheckBox{
					Key:          enableSpaceQuickLookSettingKey,
					Label:        "i18n:plugin_selection_setting_enable_space_quick_look",
					Tooltip:      "i18n:plugin_selection_setting_enable_space_quick_look_tips",
					DefaultValue: "false",
				},
				DisabledInPlatforms: []util.Platform{util.PlatformMacOS, util.PlatformLinux},
				IsPlatformSpecific:  true,
			},
		},
		Features: []plugin.MetadataFeature{
			{
				Name: plugin.MetadataFeatureQuerySelection,
			},
			{
				Name: plugin.MetadataFeatureResultPreviewWidthRatio,
				Params: map[string]any{
					// The preview command is intended to behave like Quick Look. A plugin-wide
					// WidthRatio 0 would also hide the result list for normal selection queries,
					// so the command-scoped ratio keeps only "selection preview" preview-only.
					"WidthRatio": 0.0,
					"Commands":   []string{selectionCommandPreview},
				},
			},
		},
	}
}

func (i *SelectionPlugin) Init(ctx context.Context, initParams plugin.InitParams) {
	i.api = initParams.API

	i.updateSpaceQuickLookListener(ctx, i.api.GetSetting(ctx, enableSpaceQuickLookSettingKey) == "true")
	i.api.OnSettingChanged(ctx, func(callbackCtx context.Context, key string, value string) {
		if key == enableSpaceQuickLookSettingKey {
			i.updateSpaceQuickLookListener(callbackCtx, value == "true")
		}
	})
	i.api.OnUnload(ctx, func(ctx context.Context) {
		i.stopSpaceQuickLookListener()
	})
}

// updateSpaceQuickLookListener keeps the platform monitor subscriptions aligned
// with the platform-specific plugin setting.
func (i *SelectionPlugin) updateSpaceQuickLookListener(ctx context.Context, enabled bool) {
	if !enabled || !util.IsWindows() {
		i.stopSpaceQuickLookListener()
		return
	}

	i.quickLookMu.Lock()
	if i.quickLookState != nil {
		i.quickLookMu.Unlock()
		return
	}

	state := &selectionSpaceQuickLookState{
		ops: make(chan func(), quickLookOpsCapacity),
		now: time.Now,
		openInstance: func() bool {
			return i.triggerSpaceQuickLook()
		},
		closeInstance: func() {
			i.closeSpaceQuickLook()
		},
	}
	explorerSub, explorerErr := quickjump.AddExplorerRawKeyListener(state.handleRawKey)
	if explorerErr != nil {
		i.quickLookMu.Unlock()
		i.api.Log(ctx, plugin.LogLevelWarning, "Failed to enable Space Quick Look explorer listener: "+explorerErr.Error())
		return
	}

	dialogSub, dialogErr := quickjump.AddExplorerOpenSaveRawKeyListener(state.handleRawKey)
	if dialogErr != nil {
		if explorerSub != nil {
			_ = explorerSub.Close()
		}
		i.quickLookMu.Unlock()
		i.api.Log(ctx, plugin.LogLevelWarning, "Failed to enable Space Quick Look dialog listener: "+dialogErr.Error())
		return
	}

	state.explorerSub = explorerSub
	state.dialogSub = dialogSub
	i.quickLookState = state
	i.quickLookMu.Unlock()

	util.Go(context.Background(), "selection space quick look ops", func() {
		for op := range state.ops {
			op()
		}
	})
}

// stopSpaceQuickLookListener removes any active Space Quick Look monitor
// subscriptions owned by the Selection plugin.
func (i *SelectionPlugin) stopSpaceQuickLookListener() {
	i.quickLookMu.Lock()
	state := i.quickLookState
	i.quickLookState = nil
	i.quickLookMu.Unlock()

	if state != nil {
		state.close()
	}
}

func (s *selectionSpaceQuickLookState) close() {
	if s.explorerSub != nil {
		_ = s.explorerSub.Close()
	}
	if s.dialogSub != nil {
		_ = s.dialogSub.Close()
	}

	s.mu.Lock()
	s.closed = true
	closeWanted := s.quickLookOpen
	s.quickLookOpen = false
	s.closeAfterOpen = false
	s.mu.Unlock()
	if closeWanted && s.closeInstance != nil {
		// After closed is set only this teardown path may enqueue, so the
		// channel close below is safe and the worker drains this final close.
		select {
		case s.ops <- s.closeInstance:
		default:
		}
	}
	close(s.ops)
}

// enqueue runs op on the serialized quick look worker unless the state is
// already torn down. The send is non-blocking: ops also take the state mutex,
// so a full queue must drop instead of deadlocking against the worker; the
// desired-state tracking keeps the next Space press self-healing.
func (s *selectionSpaceQuickLookState) enqueue(op func()) {
	if op == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	select {
	case s.ops <- op:
	default:
	}
	s.mu.Unlock()
}

// handleRawKey applies QuickLook-style Space filtering: short press toggles the
// preview, long press peeks (release closes), and Escape closes an open preview.
func (s *selectionSpaceQuickLookState) handleRawKey(event keyboard.RawKeyEvent) bool {
	if event.Type == keyboard.EventTypeKeyUp {
		if event.Key == keyboard.KeySpace {
			s.releaseSpace()
		}
		return false
	}

	if event.Type != keyboard.EventTypeKeyDown {
		return false
	}

	if event.Key == keyboard.KeyEscape {
		return s.closeIfOpen()
	}

	if event.Key != keyboard.KeySpace {
		s.recordInvalidKeyIfNeeded(event)
		return false
	}

	s.mu.Lock()
	if s.spaceDown {
		// Auto-repeat: keep consuming if the original press was consumed.
		consume := s.spaceConsumed
		s.mu.Unlock()
		return consume
	}

	s.spaceDown = true
	if event.Modifiers != 0 {
		s.spaceConsumed = false
		s.mu.Unlock()
		return false
	}
	now := s.now()
	if now.Before(s.invalidUntil) {
		s.spaceConsumed = false
		s.mu.Unlock()
		return false
	}
	s.spaceConsumed = true
	s.spacePressedAt = now
	shouldOpen := !s.quickLookOpen
	s.quickLookOpen = shouldOpen
	var openDispatch, closeDispatch bool
	if shouldOpen {
		s.openInFlight = true
		s.closeAfterOpen = false
		openDispatch = true
	} else if s.openInFlight {
		// A previous open is still running; its completion performs the close.
		s.closeAfterOpen = true
	} else {
		closeDispatch = true
	}
	s.mu.Unlock()

	if openDispatch {
		s.enqueueOpen()
	} else if closeDispatch {
		s.enqueue(s.closeInstance)
	}
	return true
}

// releaseSpace ends one Space press. Held presses close the preview (peek);
// short presses keep it open. Close decisions taken while the open is still in
// flight are deferred to the open completion.
func (s *selectionSpaceQuickLookState) releaseSpace() {
	s.mu.Lock()
	pressedAt := s.spacePressedAt
	s.spaceDown = false
	s.spaceConsumed = false
	s.spacePressedAt = time.Time{}
	dispatchClose := false
	if !pressedAt.IsZero() && s.quickLookOpen && s.now().Sub(pressedAt) >= spaceQuickLookHoldDelay {
		s.quickLookOpen = false
		if s.openInFlight {
			s.closeAfterOpen = true
		} else {
			dispatchClose = true
		}
	}
	s.mu.Unlock()
	if dispatchClose {
		s.enqueue(s.closeInstance)
	}
}

// closeIfOpen closes an open preview on Escape and reports whether the key was
// consumed, so Escape never reaches the underlying Explorer or dialog twice.
func (s *selectionSpaceQuickLookState) closeIfOpen() bool {
	s.mu.Lock()
	if !s.quickLookOpen {
		s.mu.Unlock()
		return false
	}
	s.quickLookOpen = false
	dispatchClose := false
	if s.openInFlight {
		s.closeAfterOpen = true
	} else {
		dispatchClose = true
	}
	s.mu.Unlock()
	if dispatchClose {
		s.enqueue(s.closeInstance)
	}
	return true
}

// enqueueOpen runs one open and reconciles the deferred close decision with
// the open result. A failed open (no valid selection) leaves no window behind,
// so the desired state falls back to closed.
func (s *selectionSpaceQuickLookState) enqueueOpen() {
	s.enqueue(func() {
		dispatched := s.openInstance()
		s.mu.Lock()
		s.openInFlight = false
		if !dispatched {
			s.quickLookOpen = false
		}
		closeAfterOpen := s.closeAfterOpen && dispatched
		s.closeAfterOpen = false
		s.mu.Unlock()
		if closeAfterOpen {
			s.closeInstance()
		}
	})
}

// recordInvalidKeyIfNeeded suppresses Space briefly after normal typing or
// command keys so typing in Explorer cannot accidentally open preview.
func (s *selectionSpaceQuickLookState) recordInvalidKeyIfNeeded(event keyboard.RawKeyEvent) {
	if isSpaceQuickLookNavigationKey(event.Key) || isSpaceQuickLookModifierKey(event.Key) {
		return
	}

	s.mu.Lock()
	s.invalidUntil = s.now().Add(time.Second)
	s.mu.Unlock()
}

func isSpaceQuickLookNavigationKey(key keyboard.Key) bool {
	switch key {
	case keyboard.KeyLeft, keyboard.KeyRight, keyboard.KeyUp, keyboard.KeyDown,
		keyboard.KeyReturn, keyboard.KeyEscape, keyboard.KeyF5, keyboard.KeyF11:
		return true
	default:
		return false
	}
}

func isSpaceQuickLookModifierKey(key keyboard.Key) bool {
	switch key {
	case keyboard.KeyCtrl, keyboard.KeyShift, keyboard.KeyAlt, keyboard.KeySuper:
		return true
	default:
		return false
	}
}

// triggerSpaceQuickLook opens the existing preview-only Selection query for a
// single selected file. It runs in a named secondary launcher session so the
// preview never overwrites the primary launcher's query state. Closing destroys
// that secondary; the next quick look opens a fresh window for the new selection.
// It reports whether an open was actually dispatched; validation failures mean
// no window appears.
func (i *SelectionPlugin) triggerSpaceQuickLook() bool {
	ctx := util.NewTraceContext()
	ctx = util.WithCoreSessionContext(ctx)
	ctx = util.WithShowSourceContext(ctx, string(common.ShowSourceSelection))

	selected, err := selection.GetSelected(ctx)
	if err != nil {
		return false
	}
	if selected.Type != selection.SelectionTypeFile || len(selected.FilePaths) != 1 {
		return false
	}
	if !util.IsFileExists(selected.FilePaths[0]) {
		return false
	}

	plugin.GetPluginManager().GetUI().OpenWoxInstance(ctx, common.OpenWoxInstanceRequest{
		Role:         common.WoxInstanceRoleSecondary,
		InstanceName: quickLookInstanceName,
		Query: common.PlainQuery{
			QueryId:        uuid.NewString(),
			QueryType:      plugin.QueryTypeSelection,
			QuerySelection: selected,
			QueryScope: common.QueryScope{
				Plugins: []common.QueryScopePlugin{{
					PluginID: selectionPluginID,
					Command:  selectionCommandPreview,
				}},
			},
		},
		ShowApp: common.ShowContext{
			HideQueryBox:        true,
			HideToolbar:         true,
			ShowPreviewTitleBar: true,
			ShowSource:          common.ShowSourceSelection,
			// The preview must not steal focus from Explorer: the raw-key pipe
			// keeps feeding Space keyup decisions only while Explorer stays
			// foreground, and long-press peek depends on it.
			ShowWithoutActivation: true,
			WindowPosition:        quickLookWindowPosition(quickLookWindowWidth, quickLookEstimatedWindowHeight),
			WindowWidth:           quickLookWindowWidth,
			MaxResultCount:        12,
		},
	})
	return true
}

// closeSpaceQuickLook closes the named preview instance. Closing an instance
// that is already gone is expected after rapid toggles and is only logged.
func (i *SelectionPlugin) closeSpaceQuickLook() {
	ctx := util.NewTraceContext()
	ctx = util.WithCoreSessionContext(ctx)
	if err := plugin.GetPluginManager().GetUI().CloseWoxInstance(ctx, quickLookInstanceName); err != nil {
		i.api.Log(ctx, plugin.LogLevelDebug, "Space Quick Look close: "+err.Error())
	}
}

const (
	quickLookWindowWidth           = 1000
	quickLookEstimatedWindowHeight = 600
)

// quickLookWindowPosition centers the preview over the active Explorer or
// open/save dialog window, following the same physical-origin convention as
// the quickjump Explorer overlays. The preview height is dynamic, so a rough
// estimate keeps the panel visually centered without waiting for layout.
func quickLookWindowPosition(windowWidth, estimatedHeight int) *common.WindowPosition {
	x, y, w, h, ok := quickjump.GetActiveExplorerRect()
	if !ok {
		x, y, w, h, ok = quickjump.GetActiveDialogRect()
	}
	if !ok || w <= 0 || h <= 0 {
		return nil
	}
	return &common.WindowPosition{
		X: x + max(0, w-windowWidth)/2,
		Y: y + max(0, h-estimatedHeight)/2,
	}
}

func (i *SelectionPlugin) Query(ctx context.Context, query plugin.Query) plugin.QueryResponse {
	if query.Type != plugin.QueryTypeSelection {
		return plugin.QueryResponse{}
	}

	if query.Selection.Type == selection.SelectionTypeText {
		return plugin.NewQueryResponse(i.queryForSelectionText(ctx, query.Selection.Text))
	}
	if query.Selection.Type == selection.SelectionTypeFile {
		return plugin.NewQueryResponse(i.queryForSelectionFile(ctx, query, query.Selection.FilePaths))
	}

	return plugin.QueryResponse{}
}

func (i *SelectionPlugin) queryForSelectionText(ctx context.Context, text string) []plugin.QueryResult {
	var results []plugin.QueryResult
	results = append(results, plugin.QueryResult{
		Title: i.api.GetTranslation(ctx, "selection_copy"),
		Icon:  icons.Get(icons.ActionCopy),
		Actions: []plugin.QueryResultAction{
			{
				Name: i.api.GetTranslation(ctx, "selection_copy_to_clipboard"),
				Icon: icons.Get(icons.ActionCopy),
				Action: func(ctx context.Context, actionContext plugin.ActionContext) {
					clipboard.WriteText(text)
				},
			},
		},
	})

	if util.IsFileExists(strings.TrimSpace(text)) {
		results = append(results, i.queryForFile(ctx, strings.TrimSpace(text))...)
	} else {
		noteAction := notesplugin.CreateNoteAction(i.api, "", text, "")
		results = append(results, plugin.QueryResult{
			Title:   noteAction.Name,
			Icon:    noteAction.Icon,
			Actions: []plugin.QueryResultAction{noteAction},
		})
	}

	return results
}

func (i *SelectionPlugin) queryForSelectionFile(ctx context.Context, query plugin.Query, filePaths []string) []plugin.QueryResult {
	// When the preview command is specified, skip all other actions and only
	// return the preview result for a single selected file. This allows users
	// to quickly open a file preview without seeing copy/open-folder options.
	if query.Command == selectionCommandPreview {
		if len(filePaths) == 1 {
			return i.queryForFilePreviewOnly(ctx, filePaths[0])
		}
		return []plugin.QueryResult{}
	}

	var results []plugin.QueryResult
	results = append(results, plugin.QueryResult{
		Title: i.api.GetTranslation(ctx, "selection_copy_path"),
		Icon:  icons.Get(icons.ActionCopy),
		Actions: []plugin.QueryResultAction{
			{
				Name: i.api.GetTranslation(ctx, "selection_copy"),
				Icon: icons.Get(icons.ActionCopy),
				Action: func(ctx context.Context, actionContext plugin.ActionContext) {
					clipboard.WriteText(strings.Join(filePaths, "\n"))
				},
			},
		},
	})
	if len(filePaths) == 1 {
		results = append(results, i.queryForFile(ctx, filePaths[0])...)
	}

	if util.IsMacOS() {
		// share with airdrop
		results = append(results, plugin.QueryResult{
			Title: i.api.GetTranslation(ctx, "selection_share_with_airdrop"),
			Icon:  icons.Get(icons.ActionAirdrop),
			Actions: []plugin.QueryResultAction{
				{
					Name: i.api.GetTranslation(ctx, "selection_share"),
					Icon: icons.Get(icons.ActionAirdrop),
					Action: func(ctx context.Context, actionContext plugin.ActionContext) {
						airdrop.Airdrop(filePaths)
					},
				},
			},
		})
	}

	return results
}

func (i *SelectionPlugin) queryForFile(ctx context.Context, filePath string) (results []plugin.QueryResult) {
	if !util.IsFileExists(filePath) {
		return
	}

	results = append(results, plugin.QueryResult{
		Title: i.api.GetTranslation(ctx, "selection_open_containing_folder"),
		Icon:  icons.Get(icons.ActionOpenContainingFolder),
		Actions: []plugin.QueryResultAction{
			{
				Name: i.api.GetTranslation(ctx, "selection_open_containing_folder"),
				Icon: icons.Get(icons.ActionOpenContainingFolder),
				Action: func(ctx context.Context, actionContext plugin.ActionContext) {
					shell.OpenFileInFolder(filePath)
				},
			},
		},
	})

	isDir := util.IsDirExists(filePath)
	executeAction := shellplugin.OpenAtDirectoryAction(i.api, filePath, isDir)
	results = append(results, plugin.QueryResult{
		Title:   executeAction.Name,
		Icon:    executeAction.Icon,
		Actions: []plugin.QueryResultAction{executeAction},
	})

	noteAction := notesplugin.CreateNoteAction(i.api, "", "", filePath)
	results = append(results, plugin.QueryResult{
		Title:   noteAction.Name,
		Icon:    noteAction.Icon,
		Actions: []plugin.QueryResultAction{noteAction},
	})

	results = append(results, plugin.QueryResult{
		Title: i.api.GetTranslation(ctx, "selection_preview"),
		Score: 1000,
		Icon:  icons.Get(icons.ActionPreview),
		Actions: []plugin.QueryResultAction{
			{
				Name: i.api.GetTranslation(ctx, "selection_preview"),
				Icon: icons.Get(icons.ActionPreview),
				Action: func(ctx context.Context, actionContext plugin.ActionContext) {
				},
			},
		},
		Preview: plugin.WoxPreview{
			PreviewType: plugin.WoxPreviewTypeFile,
			PreviewData: filePath,
			PreviewTags: []plugin.WoxPreviewTag{
				{Label: util.GetFileCreatedAt(filePath), Tooltip: "i18n:selection_created_at"},
				{Label: util.GetFileModifiedAt(filePath), Tooltip: "i18n:selection_modified_at"},
				{Label: util.GetFileSize(filePath), Tooltip: "i18n:selection_size"},
			},
		},
	})

	return
}

// queryForFilePreviewOnly returns only the preview result for a single file,
// used when the preview command is active to skip copy/open-folder actions.
func (i *SelectionPlugin) queryForFilePreviewOnly(ctx context.Context, filePath string) []plugin.QueryResult {
	if !util.IsFileExists(filePath) {
		return []plugin.QueryResult{}
	}

	return []plugin.QueryResult{
		{
			Title: i.api.GetTranslation(ctx, "selection_preview"),
			Score: 1000,
			Icon:  icons.Get(icons.ActionPreview),
			Actions: []plugin.QueryResultAction{
				{
					Name: i.api.GetTranslation(ctx, "selection_preview"),
					Icon: icons.Get(icons.ActionPreview),
					Action: func(ctx context.Context, actionContext plugin.ActionContext) {
					},
				},
			},
			Preview: plugin.WoxPreview{
				PreviewType: plugin.WoxPreviewTypeFile,
				PreviewData: filePath,
			},
		},
	}
}
