package gui

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"strings"
	"time"

	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazygit/pkg/commands"
	"github.com/jesseduffield/lazygit/pkg/commands/git_commands"
	"github.com/jesseduffield/lazygit/pkg/commands/git_config"
	"github.com/jesseduffield/lazygit/pkg/commands/models"
	"github.com/jesseduffield/lazygit/pkg/commands/oscommands"
	"github.com/jesseduffield/lazygit/pkg/common"
	"github.com/jesseduffield/lazygit/pkg/config"
	"github.com/jesseduffield/lazygit/pkg/gui/context"
	"github.com/jesseduffield/lazygit/pkg/gui/controllers"
	"github.com/jesseduffield/lazygit/pkg/gui/lbl"
	"github.com/jesseduffield/lazygit/pkg/gui/mergeconflicts"
	"github.com/jesseduffield/lazygit/pkg/gui/modes/cherrypicking"
	"github.com/jesseduffield/lazygit/pkg/gui/modes/diffing"
	"github.com/jesseduffield/lazygit/pkg/gui/modes/filtering"
	"github.com/jesseduffield/lazygit/pkg/gui/popup"
	"github.com/jesseduffield/lazygit/pkg/gui/presentation"
	"github.com/jesseduffield/lazygit/pkg/gui/presentation/authors"
	"github.com/jesseduffield/lazygit/pkg/gui/presentation/graph"
	"github.com/jesseduffield/lazygit/pkg/gui/style"
	"github.com/jesseduffield/lazygit/pkg/gui/types"
	"github.com/jesseduffield/lazygit/pkg/tasks"
	"github.com/jesseduffield/lazygit/pkg/theme"
	"github.com/jesseduffield/lazygit/pkg/updates"
	"github.com/jesseduffield/lazygit/pkg/utils"
	"gopkg.in/ozeidan/fuzzy-patricia.v3/patricia"
)

// screen sizing determines how much space your selected window takes up (window
// as in panel, not your terminal's window). Sometimes you want a bit more space
// to see the contents of a panel, and this keeps track of how much maximisation
// you've set
type WindowMaximisation int

const (
	SCREEN_NORMAL WindowMaximisation = iota
	SCREEN_HALF
	SCREEN_FULL
)

const StartupPopupVersion = 5

// OverlappingEdges determines if panel edges overlap
var OverlappingEdges = false

type ContextManager struct {
	ContextStack []types.Context
	sync.RWMutex
}

func NewContextManager(initialContext types.Context) ContextManager {
	return ContextManager{
		ContextStack: []types.Context{initialContext},
		RWMutex:      sync.RWMutex{},
	}
}

type Helpers struct {
	Refs        *controllers.RefsHelper
	Bisect      *controllers.BisectHelper
	Suggestions *controllers.SuggestionsHelper
	Files       *controllers.FilesHelper
	WorkingTree *controllers.WorkingTreeHelper
	Tags        *controllers.TagsHelper
	Rebase      *controllers.RebaseHelper
	CherryPick  *controllers.CherryPickHelper
}

type Repo string

// Gui wraps the gocui Gui object which handles rendering and events
type Gui struct {
	*common.Common
	g         *gocui.Gui
	git       *commands.GitCommand
	OSCommand *oscommands.OSCommand

	// this is the state of the GUI for the current repo
	State *GuiRepoState

	// this is a mapping of repos to gui states, so that we can restore the original
	// gui state when returning from a subrepo
	RepoStateMap         map[Repo]*GuiRepoState
	Config               config.AppConfigurer
	Updater              *updates.Updater
	statusManager        *statusManager
	credentials          credentials
	waitForIntro         sync.WaitGroup
	fileWatcher          *fileWatcher
	viewBufferManagerMap map[string]*tasks.ViewBufferManager
	stopChan             chan struct{}

	// when lazygit is opened outside a git directory we want to open to the most
	// recent repo with the recent repos popup showing
	showRecentRepos bool

	Mutexes guiMutexes

	// findSuggestions will take a string that the user has typed into a prompt
	// and return a slice of suggestions which match that string.
	findSuggestions func(string) []*types.Suggestion

	// when you enter into a submodule we'll append the superproject's path to this array
	// so that you can return to the superproject
	RepoPathStack *utils.StringStack

	// this tells us whether our views have been initially set up
	ViewsSetup bool

	Views Views

	// if we've suspended the gui (e.g. because we've switched to a subprocess)
	// we typically want to pause some things that are running like background
	// file refreshes
	PauseBackgroundThreads bool

	// Log of the commands that get run, to be displayed to the user.
	CmdLog []string

	// the extras window contains things like the command log
	ShowExtrasWindow bool

	suggestionsAsyncHandler *tasks.AsyncHandler

	PopupHandler types.IPopupHandler

	IsNewRepo bool

	// controllers define keybindings for a given context
	Controllers Controllers

	// flag as to whether or not the diff view should ignore whitespace
	IgnoreWhitespaceInDiffView bool

	// if this is true, we'll load our commits using `git log --all`
	ShowWholeGitGraph bool

	// we use this to decide whether we'll return to the original directory that
	// lazygit was opened in, or if we'll retain the one we're currently in.
	RetainOriginalDir bool

	PrevLayout PrevLayout

	c       *types.ControllerCommon
	helpers *Helpers
}

// we keep track of some stuff from one render to the next to see if certain
// things have changed
type PrevLayout struct {
	Information string
	MainWidth   int
	MainHeight  int
}

type GuiRepoState struct {
	Model *types.Model
	Modes Modes

	// Suggestions will sometimes appear when typing into a prompt
	Suggestions []*types.Suggestion
	MenuItems   []*types.MenuItem

	Updating       bool
	Panels         *panelStates
	SplitMainPanel bool

	IsRefreshingFiles bool
	Searching         searchingState
	Ptmx              *os.File
	StartupStage      StartupStage // Allows us to not load everything at once

	MainContext       types.ContextKey // used to keep the main and secondary views' contexts in sync
	ContextManager    ContextManager
	Contexts          *context.ContextTree
	ViewContextMap    map[string]types.Context
	ViewTabContextMap map[string][]context.TabContext

	// WindowViewNameMap is a mapping of windows to the current view of that window.
	// Some views move between windows for example the commitFiles view and when cycling through
	// side windows we need to know which view to give focus to for a given window
	WindowViewNameMap map[string]string

	// tells us whether we've set up our views for the current repo. We'll need to
	// do this whenever we switch back and forth between repos to get the views
	// back in sync with the repo state
	ViewsSetup bool

	// this is the message of the last failed commit attempt
	failedCommitMessage string

	ScreenMode WindowMaximisation
}

type Controllers struct {
	Submodules   *controllers.SubmodulesController
	Tags         *controllers.TagsController
	LocalCommits *controllers.LocalCommitsController
	Files        *controllers.FilesController
	Remotes      *controllers.RemotesController
	Menu         *controllers.MenuController
	Bisect       *controllers.BisectController
	Undo         *controllers.UndoController
	Sync         *controllers.SyncController
}

type listPanelState struct {
	SelectedLineIdx int
}

func (h *listPanelState) SetSelectedLineIdx(value int) {
	h.SelectedLineIdx = value
}

func (h *listPanelState) GetSelectedLineIdx() int {
	return h.SelectedLineIdx
}

// for now the staging panel state, unlike the other panel states, is going to be
// non-mutative, so that we don't accidentally end up
// with mismatches of data. We might change this in the future
type LblPanelState struct {
	*lbl.State
	SecondaryFocused bool // this is for if we show the left or right panel
}

type MergingPanelState struct {
	*mergeconflicts.State

	// UserVerticalScrolling tells us if the user has started scrolling through the file themselves
	// in which case we won't auto-scroll to a conflict.
	UserVerticalScrolling bool
}

// TODO: consider splitting this out into the window and the branches view
type branchPanelState struct {
	listPanelState
}

type remotePanelState struct {
	listPanelState
}

type remoteBranchesState struct {
	listPanelState
}

type commitPanelState struct {
	listPanelState

	LimitCommits bool
}

type reflogCommitPanelState struct {
	listPanelState
}

type subCommitPanelState struct {
	listPanelState

	// e.g. name of branch whose commits we're looking at
	refName string
}

type stashPanelState struct {
	listPanelState
}

type menuPanelState struct {
	listPanelState
	OnPress func() error
}

type submodulePanelState struct {
	listPanelState
}

type suggestionsPanelState struct {
	listPanelState
}

// as we move things to the new context approach we're going to eventually
// remove this struct altogether and store this state on the contexts.
type panelStates struct {
	Branches       *branchPanelState
	Remotes        *remotePanelState
	RemoteBranches *remoteBranchesState
	Commits        *commitPanelState
	ReflogCommits  *reflogCommitPanelState
	SubCommits     *subCommitPanelState
	Stash          *stashPanelState
	Menu           *menuPanelState
	LineByLine     *LblPanelState
	Merging        *MergingPanelState
	Submodules     *submodulePanelState
	Suggestions    *suggestionsPanelState
}

type Views struct {
	Status        *gocui.View
	Files         *gocui.View
	Branches      *gocui.View
	Commits       *gocui.View
	Stash         *gocui.View
	Main          *gocui.View
	Secondary     *gocui.View
	Options       *gocui.View
	Confirmation  *gocui.View
	Menu          *gocui.View
	Credentials   *gocui.View
	CommitMessage *gocui.View
	CommitFiles   *gocui.View
	Information   *gocui.View
	AppStatus     *gocui.View
	Search        *gocui.View
	SearchPrefix  *gocui.View
	Limit         *gocui.View
	Suggestions   *gocui.View
	Extras        *gocui.View
}

type searchingState struct {
	view         *gocui.View
	isSearching  bool
	searchString string
}

// startup stages so we don't need to load everything at once
type StartupStage int

const (
	INITIAL StartupStage = iota
	COMPLETE
)

type Modes struct {
	Filtering     filtering.Filtering
	CherryPicking *cherrypicking.CherryPicking
	Diffing       diffing.Diffing
}

// if you add a new mutex here be sure to instantiate it. We're using pointers to
// mutexes so that we can pass the mutexes to controllers.
type guiMutexes struct {
	RefreshingFilesMutex  *sync.Mutex
	RefreshingStatusMutex *sync.Mutex
	SyncMutex             *sync.Mutex
	BranchCommitsMutex    *sync.Mutex
	LineByLinePanelMutex  *sync.Mutex
	SubprocessMutex       *sync.Mutex
}

func (gui *Gui) onNewRepo(filterPath string, reuseState bool) error {
	var err error
	gui.git, err = commands.NewGitCommand(
		gui.Common,
		gui.OSCommand,
		git_config.NewStdCachedGitConfig(gui.Log),
		gui.Mutexes.SyncMutex,
	)
	if err != nil {
		return err
	}

	gui.resetState(filterPath, reuseState)

	gui.resetControllers()

	if err := gui.resetKeybindings(); err != nil {
		return err
	}

	return nil
}

// reuseState determines if we pull the repo state from our repo state map or
// just re-initialize it. For now we're only re-using state when we're going
// in and out of submodules, for the sake of having the cursor back on the submodule
// when we return.
//
// I tried out always reverting to the repo's original state but found that in fact
// it gets a bit confusing to land back in the status panel when visiting a repo
// you've already switched from. There's no doubt some easy way to make the UX
// optimal for all cases but I'm too lazy to think about what that is right now
func (gui *Gui) resetState(filterPath string, reuseState bool) {
	currentDir, err := os.Getwd()

	if reuseState {
		if err == nil {
			if state := gui.RepoStateMap[Repo(currentDir)]; state != nil {
				gui.State = state
				gui.State.ViewsSetup = false
			}
		} else {
			gui.c.Log.Error(err)
		}
	}

	contexts := gui.contextTree()

	screenMode := SCREEN_NORMAL
	var initialContext types.IListContext = contexts.Files
	if filterPath != "" {
		screenMode = SCREEN_HALF
		initialContext = contexts.BranchCommits
	}

	gui.State = &GuiRepoState{
		Model: &types.Model{
			CommitFiles:           nil,
			Files:                 make([]*models.File, 0),
			Commits:               make([]*models.Commit, 0),
			StashEntries:          make([]*models.StashEntry, 0),
			FilteredReflogCommits: make([]*models.Commit, 0),
			ReflogCommits:         make([]*models.Commit, 0),
			BisectInfo:            git_commands.NewNullBisectInfo(),
			FilesTrie:             patricia.NewTrie(),
		},

		Panels: &panelStates{
			// TODO: work out why some of these are -1 and some are 0. Last time I checked there was a good reason but I'm less certain now
			Submodules:     &submodulePanelState{listPanelState{SelectedLineIdx: -1}},
			Branches:       &branchPanelState{listPanelState{SelectedLineIdx: 0}},
			Remotes:        &remotePanelState{listPanelState{SelectedLineIdx: 0}},
			RemoteBranches: &remoteBranchesState{listPanelState{SelectedLineIdx: -1}},
			Commits:        &commitPanelState{listPanelState: listPanelState{SelectedLineIdx: 0}, LimitCommits: true},
			ReflogCommits:  &reflogCommitPanelState{listPanelState{SelectedLineIdx: 0}},
			SubCommits:     &subCommitPanelState{listPanelState: listPanelState{SelectedLineIdx: 0}, refName: ""},
			Stash:          &stashPanelState{listPanelState{SelectedLineIdx: -1}},
			Menu:           &menuPanelState{listPanelState: listPanelState{SelectedLineIdx: 0}, OnPress: nil},
			Suggestions:    &suggestionsPanelState{listPanelState: listPanelState{SelectedLineIdx: 0}},
			Merging: &MergingPanelState{
				State:                 mergeconflicts.NewState(),
				UserVerticalScrolling: false,
			},
		},
		Ptmx: nil,
		Modes: Modes{
			Filtering:     filtering.New(filterPath),
			CherryPicking: cherrypicking.New(),
			Diffing:       diffing.New(),
		},
		ViewContextMap:    contexts.InitialViewContextMap(),
		ViewTabContextMap: contexts.InitialViewTabContextMap(),
		ScreenMode:        screenMode,
		// TODO: put contexts in the context manager
		ContextManager: NewContextManager(initialContext),
		Contexts:       contexts,
	}

	gui.RepoStateMap[Repo(currentDir)] = gui.State
}

// for now the split view will always be on
// NewGui builds a new gui handler
func NewGui(
	cmn *common.Common,
	config config.AppConfigurer,
	gitConfig git_config.IGitConfig,
	updater *updates.Updater,
	showRecentRepos bool,
) (*Gui, error) {
	gui := &Gui{
		Common:                  cmn,
		Config:                  config,
		Updater:                 updater,
		statusManager:           &statusManager{},
		viewBufferManagerMap:    map[string]*tasks.ViewBufferManager{},
		showRecentRepos:         showRecentRepos,
		RepoPathStack:           &utils.StringStack{},
		RepoStateMap:            map[Repo]*GuiRepoState{},
		CmdLog:                  []string{},
		suggestionsAsyncHandler: tasks.NewAsyncHandler(),

		// originally we could only hide the command log permanently via the config
		// but now we do it via state. So we need to still support the config for the
		// sake of backwards compatibility. We're making use of short circuiting here
		ShowExtrasWindow: cmn.UserConfig.Gui.ShowCommandLog && !config.GetAppState().HideCommandLog,
		Mutexes: guiMutexes{
			RefreshingFilesMutex:  &sync.Mutex{},
			RefreshingStatusMutex: &sync.Mutex{},
			SyncMutex:             &sync.Mutex{},
			BranchCommitsMutex:    &sync.Mutex{},
			LineByLinePanelMutex:  &sync.Mutex{},
			SubprocessMutex:       &sync.Mutex{},
		},
	}

	guiIO := oscommands.NewGuiIO(
		cmn.Log,
		gui.LogCommand,
		gui.getCmdWriter,
		gui.promptUserForCredential,
	)

	osCommand := oscommands.NewOSCommand(cmn, oscommands.GetPlatform(), guiIO)

	gui.OSCommand = osCommand

	gui.watchFilesForChanges()

	gui.PopupHandler = popup.NewPopupHandler(
		cmn,
		gui.createPopupPanel,
		func() error { return gui.c.Refresh(types.RefreshOptions{Mode: types.ASYNC}) },
		func() error { return gui.closeConfirmationPrompt(false) },
		gui.createMenu,
		gui.withWaitingStatus,
		gui.toast,
		func() string { return gui.Views.Confirmation.TextArea.GetContent() },
	)

	guiCommon := &guiCommon{gui: gui, IPopupHandler: gui.PopupHandler}
	controllerCommon := &types.ControllerCommon{IGuiCommon: guiCommon, Common: cmn}

	// storing this stuff on the gui for now to ease refactoring
	// TODO: reset these controllers upon changing repos due to state changing
	gui.c = controllerCommon

	authors.SetCustomAuthors(gui.UserConfig.Gui.AuthorColors)
	presentation.SetCustomBranches(gui.UserConfig.Gui.BranchColors)

	return gui, nil
}

func (gui *Gui) resetControllers() {
	controllerCommon := gui.c
	osCommand := gui.OSCommand
	rebaseHelper := controllers.NewRebaseHelper(controllerCommon, gui.State.Contexts, gui.git, gui.takeOverMergeConflictScrolling)
	model := gui.State.Model
	gui.helpers = &Helpers{
		Refs: controllers.NewRefsHelper(
			controllerCommon,
			gui.git,
			gui.State.Contexts,
			func() { gui.State.Panels.Commits.LimitCommits = true },
		),
		Bisect:      controllers.NewBisectHelper(controllerCommon, gui.git),
		Suggestions: controllers.NewSuggestionsHelper(controllerCommon, model, gui.refreshSuggestions),
		Files:       controllers.NewFilesHelper(controllerCommon, gui.git, osCommand),
		WorkingTree: controllers.NewWorkingTreeHelper(model),
		Tags:        controllers.NewTagsHelper(controllerCommon, gui.git),
		Rebase:      rebaseHelper,
		CherryPick: controllers.NewCherryPickHelper(
			controllerCommon,
			gui.git,
			gui.State.Contexts,
			func() *cherrypicking.CherryPicking { return gui.State.Modes.CherryPicking },
			rebaseHelper,
		),
	}

	syncController := controllers.NewSyncController(
		controllerCommon,
		gui.git,
		gui.getCheckedOutBranch,
		gui.helpers.Suggestions,
		gui.getSuggestedRemote,
		gui.helpers.Rebase.CheckMergeOrRebase,
	)

	submodulesController := controllers.NewSubmodulesController(
		controllerCommon,
		gui.State.Contexts.Submodules,
		gui.git,
		gui.enterSubmodule,
		gui.getSelectedSubmodule,
	)

	gui.Controllers = Controllers{
		Submodules: submodulesController,
		Files: controllers.NewFilesController(
			controllerCommon,
			gui.State.Contexts.Files,
			model,
			gui.git,
			osCommand,
			gui.getSelectedFileNode,
			gui.State.Contexts,
			gui.enterSubmodule,
			func() []*models.SubmoduleConfig { return gui.State.Model.Submodules },
			gui.getSetTextareaTextFn(func() *gocui.View { return gui.Views.CommitMessage }),
			gui.withGpgHandling,
			func() string { return gui.State.failedCommitMessage },
			gui.getSelectedPath,
			gui.switchToMerge,
			gui.helpers.Suggestions,
			gui.helpers.Refs,
			gui.helpers.Files,
			gui.helpers.WorkingTree,
		),
		Tags: controllers.NewTagsController(
			controllerCommon,
			gui.State.Contexts.Tags,
			gui.git,
			gui.State.Contexts,
			gui.helpers.Tags,
			gui.helpers.Refs,
			gui.helpers.Suggestions,
			gui.switchToSubCommitsContext,
		),
		LocalCommits: controllers.NewLocalCommitsController(
			controllerCommon,
			gui.State.Contexts.BranchCommits,
			osCommand,
			gui.git,
			gui.helpers.Tags,
			gui.helpers.Refs,
			gui.helpers.CherryPick,
			gui.helpers.Rebase,
			gui.getSelectedLocalCommit,
			model,
			func() int { return gui.State.Panels.Commits.SelectedLineIdx },
			gui.helpers.Rebase.CheckMergeOrRebase,
			syncController.HandlePull,
			gui.getHostingServiceMgr,
			gui.SwitchToCommitFilesContext,
			func() bool { return gui.State.Panels.Commits.LimitCommits },
			func(value bool) { gui.State.Panels.Commits.LimitCommits = value },
			func() bool { return gui.ShowWholeGitGraph },
			func(value bool) { gui.ShowWholeGitGraph = value },
		),
		Remotes: controllers.NewRemotesController(
			controllerCommon,
			gui.State.Contexts.Remotes,
			gui.git,
			gui.State.Contexts,
			gui.getSelectedRemote,
			func(branches []*models.RemoteBranch) { gui.State.Model.RemoteBranches = branches },
		),
		Menu: controllers.NewMenuController(
			controllerCommon,
			gui.State.Contexts.Menu,
			gui.getSelectedMenuItem,
		),
		Bisect: controllers.NewBisectController(
			controllerCommon,
			gui.State.Contexts.BranchCommits,
			gui.git,
			gui.helpers.Bisect,
			gui.getSelectedLocalCommit,
			func() []*models.Commit { return gui.State.Model.Commits },
		),
		Undo: controllers.NewUndoController(
			controllerCommon,
			gui.git,
			gui.helpers.Refs,
			gui.helpers.WorkingTree,
			func() []*models.Commit { return gui.State.Model.FilteredReflogCommits },
		),
		Sync: syncController,
	}

	gui.State.Contexts.Submodules.AddKeybindingsFn(gui.Controllers.Submodules.GetKeybindings)
	gui.State.Contexts.Files.AddKeybindingsFn(gui.Controllers.Files.GetKeybindings)
	gui.State.Contexts.Tags.AddKeybindingsFn(gui.Controllers.Tags.GetKeybindings)
	// TODO: commit to one name here: local commits or branch commits
	gui.State.Contexts.BranchCommits.AddKeybindingsFn(gui.Controllers.LocalCommits.GetKeybindings)
	gui.State.Contexts.BranchCommits.AddKeybindingsFn(gui.Controllers.Bisect.GetKeybindings)
	gui.State.Contexts.Remotes.AddKeybindingsFn(gui.Controllers.Remotes.GetKeybindings)
	gui.State.Contexts.Menu.AddKeybindingsFn(gui.Controllers.Menu.GetKeybindings)
	gui.State.Contexts.Menu.AddKeybindingsFn(gui.Controllers.Menu.GetKeybindings)
	// TODO: handle global contexts
}

var RuneReplacements = map[rune]string{
	// for the commit graph
	graph.MergeSymbol:  "M",
	graph.CommitSymbol: "o",
}

func (gui *Gui) initGocui() (*gocui.Gui, error) {
	recordEvents := recordingEvents()
	playMode := gocui.NORMAL
	if recordEvents {
		playMode = gocui.RECORDING
	} else if replaying() {
		playMode = gocui.REPLAYING
	}

	g, err := gocui.NewGui(gocui.OutputTrue, OverlappingEdges, playMode, headless(), RuneReplacements)
	if err != nil {
		return nil, err
	}

	return g, nil
}

// Run: setup the gui with keybindings and start the mainloop
func (gui *Gui) Run(filterPath string) error {
	g, err := gui.initGocui()
	if err != nil {
		return err
	}

	gui.g = g
	defer gui.g.Close()

	if replaying() {
		gui.g.RecordingConfig = gocui.RecordingConfig{
			Speed:  getRecordingSpeed(),
			Leeway: 100,
		}

		var err error
		gui.g.Recording, err = gui.loadRecording()
		if err != nil {
			return err
		}

		go utils.Safe(func() {
			time.Sleep(time.Second * 40)
			log.Fatal("40 seconds is up, lazygit recording took too long to complete")
		})
	}

	gui.g.OnSearchEscape = gui.onSearchEscape
	if err := gui.Config.ReloadUserConfig(); err != nil {
		return nil
	}
	userConfig := gui.UserConfig
	gui.g.SearchEscapeKey = gui.getKey(userConfig.Keybinding.Universal.Return)
	gui.g.NextSearchMatchKey = gui.getKey(userConfig.Keybinding.Universal.NextMatch)
	gui.g.PrevSearchMatchKey = gui.getKey(userConfig.Keybinding.Universal.PrevMatch)

	gui.g.ShowListFooter = userConfig.Gui.ShowListFooter

	if userConfig.Gui.MouseEvents {
		gui.g.Mouse = true
	}

	if err := gui.setColorScheme(); err != nil {
		return err
	}

	gui.g.SetManager(gocui.ManagerFunc(gui.layout), gocui.ManagerFunc(gui.getFocusLayout()))

	// onNewRepo must be called after g.SetManager because SetManager deletes keybindings
	if err := gui.onNewRepo(filterPath, false); err != nil {
		return err
	}

	gui.waitForIntro.Add(1)
	if gui.c.UserConfig.Git.AutoFetch {
		go utils.Safe(gui.startBackgroundFetch)
	}

	gui.goEvery(time.Second*time.Duration(userConfig.Refresher.RefreshInterval), gui.stopChan, gui.refreshFilesAndSubmodules)

	gui.c.Log.Info("starting main loop")

	return gui.g.MainLoop()
}

func (gui *Gui) RunAndHandleError(filterPath string) error {
	gui.stopChan = make(chan struct{})
	return utils.SafeWithError(func() error {
		if err := gui.Run(filterPath); err != nil {
			for _, manager := range gui.viewBufferManagerMap {
				manager.Close()
			}

			if !gui.fileWatcher.Disabled {
				gui.fileWatcher.Watcher.Close()
			}

			close(gui.stopChan)

			switch err {
			case gocui.ErrQuit:
				if !gui.RetainOriginalDir {
					if err := gui.recordCurrentDirectory(); err != nil {
						return err
					}
				}

				if err := gui.saveRecording(gui.g.Recording); err != nil {
					return err
				}

				return nil

			default:
				return err
			}
		}

		return nil
	})
}

// returns whether command exited without error or not
func (gui *Gui) runSubprocessWithSuspenseAndRefresh(subprocess oscommands.ICmdObj) error {
	_, err := gui.runSubprocessWithSuspense(subprocess)
	if err != nil {
		return err
	}

	if err := gui.c.Refresh(types.RefreshOptions{Mode: types.ASYNC}); err != nil {
		return err
	}

	return nil
}

// returns whether command exited without error or not
func (gui *Gui) runSubprocessWithSuspense(subprocess oscommands.ICmdObj) (bool, error) {
	gui.Mutexes.SubprocessMutex.Lock()
	defer gui.Mutexes.SubprocessMutex.Unlock()

	if replaying() {
		// we do not yet support running subprocesses within integration tests. So if
		// we're replaying an integration test and we're inside this method, something
		// has gone wrong, so we should fail

		log.Fatal("opening subprocesses not yet supported in integration tests. Chances are that this test is running too fast and a subprocess is accidentally opened")
	}

	if err := gui.g.Suspend(); err != nil {
		return false, gui.c.Error(err)
	}

	gui.PauseBackgroundThreads = true

	cmdErr := gui.runSubprocess(subprocess)

	if err := gui.g.Resume(); err != nil {
		return false, err
	}

	gui.PauseBackgroundThreads = false

	if cmdErr != nil {
		return false, gui.c.Error(cmdErr)
	}

	return true, nil
}

func (gui *Gui) runSubprocess(cmdObj oscommands.ICmdObj) error { //nolint:unparam
	gui.LogCommand(cmdObj.ToString(), true)

	subprocess := cmdObj.GetCmd()
	subprocess.Stdout = os.Stdout
	subprocess.Stderr = os.Stdout
	subprocess.Stdin = os.Stdin

	fmt.Fprintf(os.Stdout, "\n%s\n\n", style.FgBlue.Sprint("+ "+strings.Join(subprocess.Args, " ")))

	err := subprocess.Run()

	subprocess.Stdout = ioutil.Discard
	subprocess.Stderr = ioutil.Discard
	subprocess.Stdin = nil

	fmt.Fprintf(os.Stdout, "\n%s\n", style.FgGreen.Sprint(gui.c.Tr.PressEnterToReturn))
	fmt.Scanln() // wait for enter press

	return err
}

func (gui *Gui) loadNewRepo() error {
	if err := gui.updateRecentRepoList(); err != nil {
		return err
	}

	if err := gui.c.Refresh(types.RefreshOptions{Mode: types.ASYNC}); err != nil {
		return err
	}

	if err := gui.OSCommand.UpdateWindowTitle(); err != nil {
		return err
	}

	return nil
}

func (gui *Gui) showInitialPopups(tasks []func(chan struct{}) error) {
	gui.waitForIntro.Add(len(tasks))
	done := make(chan struct{})

	go utils.Safe(func() {
		for _, task := range tasks {
			task := task
			go utils.Safe(func() {
				if err := task(done); err != nil {
					_ = gui.c.Error(err)
				}
			})

			<-done
			gui.waitForIntro.Done()
		}
	})
}

func (gui *Gui) showIntroPopupMessage(done chan struct{}) error {
	onConfirm := func() error {
		done <- struct{}{}
		gui.c.GetAppState().StartupPopupVersion = StartupPopupVersion
		return gui.c.SaveAppState()
	}

	return gui.c.Ask(types.AskOpts{
		Title:         "",
		Prompt:        gui.c.Tr.IntroPopupMessage,
		HandleConfirm: onConfirm,
		HandleClose:   onConfirm,
	})
}

func (gui *Gui) goEvery(interval time.Duration, stop chan struct{}, function func() error) {
	go utils.Safe(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if gui.PauseBackgroundThreads {
					continue
				}
				_ = function()
			case <-stop:
				return
			}
		}
	})
}

func (gui *Gui) startBackgroundFetch() {
	gui.waitForIntro.Wait()
	isNew := gui.IsNewRepo
	userConfig := gui.UserConfig
	if !isNew {
		time.After(time.Duration(userConfig.Refresher.FetchInterval) * time.Second)
	}
	err := gui.backgroundFetch()
	if err != nil && strings.Contains(err.Error(), "exit status 128") && isNew {
		_ = gui.c.Ask(types.AskOpts{
			Title:  gui.c.Tr.NoAutomaticGitFetchTitle,
			Prompt: gui.c.Tr.NoAutomaticGitFetchBody,
		})
	} else {
		gui.goEvery(time.Second*time.Duration(userConfig.Refresher.FetchInterval), gui.stopChan, func() error {
			err := gui.backgroundFetch()
			gui.render()
			return err
		})
	}
}

// setColorScheme sets the color scheme for the app based on the user config
func (gui *Gui) setColorScheme() error {
	userConfig := gui.UserConfig
	theme.UpdateTheme(userConfig.Gui.Theme)

	gui.g.FgColor = theme.InactiveBorderColor
	gui.g.SelFgColor = theme.ActiveBorderColor
	gui.g.FrameColor = theme.InactiveBorderColor
	gui.g.SelFrameColor = theme.ActiveBorderColor

	return nil
}

func (gui *Gui) OnUIThread(f func() error) {
	gui.g.Update(func(*gocui.Gui) error {
		return f()
	})
}
