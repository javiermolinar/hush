package tui

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/javiermolinar/hush/tui/extension"
)

type registeredAction struct {
	owner        string
	action       extension.Action
	bindingLabel string
	skill        stagedSkill
}
type segmentRank struct {
	key             string
	priority, order int
	slot            extension.SegmentSlot
}

func compareSegmentLayout(a, b segmentRank) int {
	if a.slot != b.slot {
		return cmp.Compare(a.slot, b.slot)
	}
	if a.order != b.order {
		return cmp.Compare(a.order, b.order)
	}
	return cmp.Compare(a.key, b.key)
}

func compareSegmentPriority(a, b segmentRank) int {
	if a.priority != b.priority {
		return cmp.Compare(b.priority, a.priority)
	}
	// On ties keep the piece drawn earlier, as the old eviction loop did.
	return compareSegmentLayout(a, b)
}

type segmentSpec struct {
	rank   segmentRank
	render func(extension.RenderContext) extension.Line
}
type widgetSpec struct {
	key string
	extension.Widget
}
type activitySpec struct {
	key string
	extension.Summarizer
}
type registry struct {
	registrations         []extension.Registration
	actions               []registeredAction
	byID                  map[extension.ActionID]int
	direct, leader, slash map[string]int
	slashOrder            []int
	events                map[extension.EventKind][]func(extension.Context, extension.Event)
	entries               map[string]extension.EntryRenderer
	tools                 map[string]extension.ToolRenderer
	customEntries         map[string]bool
	customTools           bool
	activity              []activitySpec
	statusSegments        []segmentSpec
	tabSegments           []segmentSpec
	widgets               []widgetSpec
	startupErr            error
}

var apiVersion = regexp.MustCompile(`^0\.1\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var ident = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
var slashName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// slashRank orders prefix matches, which Enter runs first. Themes change
// rarely, so /theme sorts after other commands and a shared prefix such
// as /th keeps opening /thinking.
func slashRank(id extension.ActionID) int {
	if id == themesID {
		return 1
	}
	return 0
}

func compatibleVersion(version string) bool {
	match := apiVersion.FindStringSubmatch(version)
	if match == nil {
		return false
	}
	for _, v := range strings.Split(match[3], ".") {
		if len(v) > 1 && v[0] == '0' && strings.Trim(v, "0123456789") == "" {
			return false
		}
	}
	return true
}

func normalizeChord(key string) string {
	p := strings.Split(strings.ToLower(strings.TrimSpace(key)), "+")
	if len(p) == 0 || p[len(p)-1] == "" {
		return ""
	}
	var ctr, alt, shift, super bool
	for _, mod := range p[:len(p)-1] {
		switch mod {
		case "ctrl":
			if ctr {
				return ""
			}
			ctr = true
		case "alt":
			if alt {
				return ""
			}
			alt = true
		case "shift":
			if shift {
				return ""
			}
			shift = true
		case "super":
			if super {
				return ""
			}
			super = true
		default:
			return ""
		}
	}
	base := p[len(p)-1]
	if strings.ContainsAny(base, " \t\n\r") {
		return ""
	}
	s := ""
	if ctr {
		s += "ctrl+"
	}
	if alt {
		s += "alt+"
	}
	if shift {
		s += "shift+"
	}
	if super {
		s += "super+"
	}
	return s + base
}

func makeRegistry(options Options) (registry, error) {
	r := registry{byID: make(map[extension.ActionID]int), direct: make(map[string]int), leader: make(map[string]int), slash: make(map[string]int),
		events: make(map[extension.EventKind][]func(extension.Context, extension.Event)), entries: make(map[string]extension.EntryRenderer), tools: make(map[string]extension.ToolRenderer), customEntries: make(map[string]bool)}
	if len(options.Extensions) > 64 {
		return r, fmt.Errorf("tui: too many extensions")
	}
	intercepted := make(map[string]bool, len(options.InterceptedKeys))
	for _, key := range options.InterceptedKeys {
		intercepted[normalizeChord(key)] = true
	}
	ids := make(map[string]bool)
	for _, reg := range append([]extension.Registration{builtins(options.WorkspaceTrust != nil), footerRegistration(), activityRegistration(), menusRegistration(options)}, options.Extensions...) {
		fail := func(what string) (registry, error) {
			return registry{}, fmt.Errorf("tui extension %q: %s", reg.ID, what)
		}
		if !ident.MatchString(reg.ID) || ids[reg.ID] || !compatibleVersion(reg.APIVersion) {
			return fail("invalid or duplicate ID, or incompatible API version")
		}
		ids[reg.ID] = true
		if len(reg.Actions)+len(reg.Events)+len(reg.Entries)+len(reg.Tools)+len(reg.Segments)+len(reg.Widgets)+len(reg.Activity) > 256 {
			return fail("contribution limit")
		}
		r.registrations = append(r.registrations, extension.Registration{ID: reg.ID, Init: reg.Init, Shutdown: reg.Shutdown})
		for _, a := range reg.Actions {
			local, qualified := strings.CutPrefix(string(a.ID), reg.ID+".")
			if _, exists := r.byID[a.ID]; !qualified || !ident.MatchString(local) || exists || a.Run == nil || strings.TrimSpace(a.Title) == "" {
				return fail("invalid or duplicate action")
			}
			i := len(r.actions)
			if a.Slash != nil {
				s := *a.Slash
				if _, exists := r.slash[s.Name]; exists || !slashName.MatchString(s.Name) {
					return fail("invalid or duplicate slash name")
				}
				a.Slash = &s
				r.slash[s.Name] = i
				r.slashOrder = append(r.slashOrder, i)
			}
			if override, ok := options.Keymap[a.ID]; ok {
				a.Binding = override
			}
			if a.Binding.Direct != "" {
				key := normalizeChord(a.Binding.Direct)
				if _, exists := r.direct[key]; exists || !validDirect(key) {
					return fail("reserved, invalid or duplicate direct key: " + a.Binding.Direct)
				}
				a.Binding.Direct = key
				r.direct[key] = i
			}
			keys := strings.Fields(a.Binding.Leader)
			if len(keys) == 0 || len(keys) > 4 {
				return fail("a leader sequence of one to four chords is required")
			}
			for j, key := range keys {
				keys[j] = normalizeChord(key)
				if keys[j] == "" || keys[j] == "esc" || keys[j] == "ctrl+x" || keys[j] == "ctrl+c" {
					return fail("invalid or reserved leader key")
				}
			}
			seq := strings.Join(keys, " ")
			for existing := range r.leader {
				if seq == existing || strings.HasPrefix(seq, existing+" ") || strings.HasPrefix(existing, seq+" ") {
					return fail("leader sequence conflict: " + seq)
				}
			}
			a.Binding.Leader = seq
			label := a.Binding.Direct
			if label == "" || intercepted[label] {
				label = "ctrl+x " + seq
			}
			a.Title = limitText(a.Title, 256)
			a.Description = limitText(a.Description, 1024)
			r.actions = append(r.actions, registeredAction{owner: reg.ID, action: a, bindingLabel: label})
			r.byID[a.ID] = i
			r.leader[seq] = i
		}
		for _, s := range reg.Events {
			switch s.Kind {
			case extension.ConnectionChanged, extension.ThreadSwitched, extension.EntryObserved, extension.StreamChanged, extension.ToolStarted, extension.ToolOutput, extension.ToolResult, extension.StatusChanged, extension.UsageChanged, extension.ApprovalChanged:
			default:
				return fail("invalid event kind")
			}
			if s.Observe == nil {
				return fail("missing observer")
			}
			r.events[s.Kind] = append(r.events[s.Kind], s.Observe)
		}
		for _, s := range reg.Entries {
			if _, ok := r.entries[s.Kind]; !ident.MatchString(s.Kind) || ok || s.Render == nil {
				return fail("invalid or duplicate entry kind")
			}
			r.entries[s.Kind] = s
			r.customEntries[s.Kind] = reg.ID != coreID
		}
		for _, s := range reg.Tools {
			if _, ok := r.tools[s.Name]; !ident.MatchString(s.Name) || ok || s.Render == nil {
				return fail("invalid or duplicate tool name")
			}
			r.tools[s.Name] = s
			r.customTools = r.customTools || reg.ID != coreID
		}
		keys := make(map[string]bool)
		for _, s := range reg.Segments {
			if !ident.MatchString(s.Key) || keys[s.Key] || s.Render == nil || s.Slot > extension.Tab {
				return fail("invalid or duplicate segment")
			}
			keys[s.Key] = true
			qualified := reg.ID + ":" + s.Key
			if order, ok := options.SegmentOrder[qualified]; ok {
				s.Order = order
			}
			spec := segmentSpec{segmentRank{qualified, s.Priority, s.Order, s.Slot}, s.Render}
			if s.Slot == extension.Tab {
				r.tabSegments = append(r.tabSegments, spec)
			} else {
				r.statusSegments = append(r.statusSegments, spec)
			}
		}
		clear(keys)
		for _, s := range reg.Widgets {
			if !ident.MatchString(s.Key) || keys[s.Key] || s.Render == nil || s.Placement > extension.BelowEditor {
				return fail("invalid or duplicate widget")
			}
			keys[s.Key] = true
			r.widgets = append(r.widgets, widgetSpec{reg.ID + ":" + s.Key, s})
		}
		clear(keys)
		for _, s := range reg.Activity {
			if !ident.MatchString(s.Key) || keys[s.Key] || s.Summarize == nil {
				return fail("invalid or duplicate summarizer")
			}
			keys[s.Key] = true
			r.activity = append(r.activity, activitySpec{reg.ID + ":" + s.Key, s})
		}
	}
	for id := range options.Keymap {
		if _, ok := r.byID[id]; !ok {
			return registry{}, fmt.Errorf("tui: remap for unknown action %q", id)
		}
	}
	slices.SortFunc(r.slashOrder, func(a, b int) int {
		first, second := r.actions[a].action, r.actions[b].action
		return cmp.Or(cmp.Compare(slashRank(first.ID), slashRank(second.ID)), cmp.Compare(first.Slash.Name, second.Slash.Name))
	})
	for _, group := range [][]segmentSpec{r.statusSegments, r.tabSegments} {
		slices.SortFunc(group, func(a, b segmentSpec) int { return compareSegmentPriority(a.rank, b.rank) })
	}
	slices.SortFunc(r.widgets, func(a, b widgetSpec) int {
		if n := cmp.Compare(a.Order, b.Order); n != 0 {
			return n
		}
		return cmp.Compare(a.key, b.key)
	})
	slices.SortFunc(r.activity, func(a, b activitySpec) int {
		if n := cmp.Compare(a.Order, b.Order); n != 0 {
			return n
		}
		return cmp.Compare(a.key, b.key)
	})
	return r, nil
}

func validDirect(chord string) bool {
	if !strings.Contains(chord, "+") || strings.HasPrefix(chord, "shift+") && chord != "shift+tab" {
		return false
	}
	switch chord {
	case "ctrl+x", "ctrl+j", "ctrl+c", "ctrl+d", "ctrl+home", "ctrl+end", "alt+enter", "ctrl+z", "ctrl+k", "alt+[":
		return false
	}
	return true
}
