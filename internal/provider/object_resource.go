// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JamesonRGrieve/tofu-jetstream/internal/jetstream"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*objectResource)(nil)
	_ resource.ResourceWithConfigure   = (*objectResource)(nil)
	_ resource.ResourceWithImportState = (*objectResource)(nil)
)

// globalID is the resource id used for the global (no-context) config block.
const globalID = "(global)"

// NewObjectResource constructs the generic jetstream_object resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct {
	client *jetstream.Client
}

// objectModel is the state/plan shape for jetstream_object.
//
//   - Context  — the running-config block to manage (e.g. "vlan 2",
//     "interface gigabitEthernet 1/0/6"); empty/null = the global config context.
//   - Lines    — JSON array of the managed config lines within the block. These
//     are the ONLY lines this resource touches (manage-declared-only).
//   - Previous — computed snapshot of the block's lines at create/import, used to
//     restore exactly on destroy (only lines we added are negated).
//   - ID       — the normalized context (or "(global)").
type objectModel struct {
	ID       types.String `tfsdk:"id"`
	Context  types.String `tfsdk:"context"`
	Lines    types.String `tfsdk:"lines"`
	Previous types.String `tfsdk:"previous"`
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A generic JetStream running-config block. `context` is the block to manage " +
			"(`vlan 2`, `interface gigabitEthernet 1/0/6`, `interface vlan 1`; empty for global config) and " +
			"`lines` is a JSON array of the config lines this resource manages within that block. On " +
			"create/update the declared lines are entered under the context, then `copy running-config " +
			"startup-config` saves. **Manage-declared-only:** only the lines in `lines` are ever touched; " +
			"every other line in the block (and the rest of the config) is left alone. A subset plan modifier " +
			"suppresses the diff when every declared line already appears in the block on the device, so an " +
			"existing config imports to 0-diff and unmanaged config is never clobbered. On destroy, each " +
			"declared line that this resource added (not present at create/import) is negated with `no …`, " +
			"then saved.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource id — the normalized `context` (or `(global)`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"context": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The running-config block (context) to manage, e.g. `vlan 2`, " +
					"`interface vlan 1`, `interface gigabitEthernet 1/0/6`. Omit for the global config context " +
					"(lines entered directly in `configure` mode). Changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"lines": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON array of the config lines managed within the block, e.g. " +
					"`jsonencode([\"switchport general allowed vlan 2-3 tagged\"])`. Only these lines are " +
					"managed; all other lines in the block are left alone.",
				PlanModifiers: []planmodifier.String{lineSubsetSuppress{}},
			},
			"previous": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Computed snapshot of the block's lines captured at create/import (JSON " +
					"array). Used to restore exactly on destroy: only declared lines absent from this snapshot " +
					"(the ones this resource added) are negated.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *objectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*jetstream.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *jetstream.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	declared, err := parseLines(m.Lines.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid lines", err.Error())
		return
	}
	cfgCtx := m.Context.ValueString()
	// Snapshot the prior block so destroy can restore exactly.
	prior, _, err := r.client.Block(ctx, cfgCtx)
	if err != nil {
		resp.Diagnostics.AddError("JetStream read (snapshot) failed", err.Error())
		return
	}
	if err := r.client.ApplyLines(ctx, cfgCtx, declared); err != nil {
		resp.Diagnostics.AddError("JetStream apply failed", err.Error())
		return
	}
	m.ID = types.StringValue(contextID(cfgCtx))
	m.Previous = types.StringValue(marshalLines(prior))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cfgCtx := m.Context.ValueString()
	live, _, err := r.client.Block(ctx, cfgCtx)
	if err != nil {
		resp.Diagnostics.AddError("JetStream read failed", err.Error())
		return
	}
	// Store the full live block; the subset plan modifier reconciles it against
	// the declared config lines at plan time (an absent block reads as [], which
	// surfaces as an update to re-apply the declared lines).
	m.Lines = types.StringValue(marshalLines(live))
	m.ID = types.StringValue(contextID(cfgCtx))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	declared, err := parseLines(plan.Lines.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid lines", err.Error())
		return
	}
	cfgCtx := plan.Context.ValueString()
	prev, _ := parseLines(state.Previous.ValueString())
	oldDeclared, _ := parseLines(state.Lines.ValueString())

	// Lines we previously added (not in the prior snapshot) that are no longer
	// declared are negated — they leave management cleanly without touching
	// pre-existing config.
	dropped := addedLines(subtractLines(oldDeclared, declared), prev)
	if len(dropped) > 0 {
		if err := r.client.RemoveLines(ctx, cfgCtx, dropped); err != nil {
			resp.Diagnostics.AddError("JetStream restore (dropped lines) failed", err.Error())
			return
		}
	}
	if err := r.client.ApplyLines(ctx, cfgCtx, declared); err != nil {
		resp.Diagnostics.AddError("JetStream apply failed", err.Error())
		return
	}
	plan.ID = types.StringValue(contextID(cfgCtx))
	plan.Previous = state.Previous
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *objectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	declared, err := parseLines(m.Lines.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid lines in state", err.Error())
		return
	}
	prev, _ := parseLines(m.Previous.ValueString())
	// Negate only the lines this resource added (absent at create/import).
	added := addedLines(declared, prev)
	if len(added) == 0 {
		return // everything pre-existed — adoption-safe no-op
	}
	if err := r.client.RemoveLines(ctx, m.Context.ValueString(), added); err != nil {
		resp.Diagnostics.AddError("JetStream restore failed", err.Error())
	}
}

func (r *objectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id is the config-block context verbatim, e.g.
	//   tofu import jetstream_object.p6 'interface gigabitEthernet 1/0/6'
	// or the literal "(global)" for the global context. `lines` and `previous`
	// are populated from the live block so the import lands adoption-safe
	// (previous == the full live block, so destroy negates nothing).
	if r.client == nil {
		resp.Diagnostics.AddError("Provider not configured", "import requires a configured provider client")
		return
	}
	raw := strings.TrimSpace(req.ID)
	cfgCtx := raw
	if raw == globalID {
		cfgCtx = ""
	}
	live, _, err := r.client.Block(ctx, cfgCtx)
	if err != nil {
		resp.Diagnostics.AddError("JetStream import read failed", err.Error())
		return
	}
	if cfgCtx == "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("context"), types.StringNull())...)
	} else {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("context"), jetstream.NormalizeLine(cfgCtx))...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), contextID(cfgCtx))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("lines"), marshalLines(live))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("previous"), marshalLines(live))...)
}

// ---------------------------------------------------------------------------
// Pure helpers — line JSON encode/decode, set math, id derivation. Unit-tested.
// ---------------------------------------------------------------------------

// parseLines parses the `lines` JSON array into a string slice. An empty string
// is an empty slice; a non-array or non-string element is an error.
func parseLines(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("`lines` must be a JSON array of strings: %w", err)
	}
	return out, nil
}

// marshalLines serializes a line slice as a compact JSON array, preserving order.
func marshalLines(lines []string) string {
	if lines == nil {
		lines = []string{}
	}
	out, err := json.Marshal(lines)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// normalizedSet returns a set of the normalized forms of lines.
func normalizedSet(lines []string) map[string]struct{} {
	set := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		set[jetstream.NormalizeLine(l)] = struct{}{}
	}
	return set
}

// subtractLines returns the lines in a whose normalized form is not in b.
func subtractLines(a, b []string) []string {
	bset := normalizedSet(b)
	var out []string
	for _, l := range a {
		if _, ok := bset[jetstream.NormalizeLine(l)]; !ok {
			out = append(out, l)
		}
	}
	return out
}

// addedLines returns the declared lines whose normalized form is not in the
// prior snapshot (i.e. the lines this resource introduced).
func addedLines(declared, prior []string) []string {
	return subtractLines(declared, prior)
}

// contextID derives the resource id from a context: the normalized context, or
// "(global)" for the empty context.
func contextID(ctx string) string {
	n := jetstream.NormalizeLine(ctx)
	if n == "" {
		return globalID
	}
	return n
}

// ---------------------------------------------------------------------------
// subset / no-op plan modifier — suppress the diff on `lines` when every declared
// line already appears in the block on the device (the live block held in prior
// state). This is what lets a declared subset import/refresh to 0-diff without
// touching unmanaged config.
// ---------------------------------------------------------------------------

type lineSubsetSuppress struct{}

func (lineSubsetSuppress) Description(context.Context) string {
	return "Suppress diff when every declared line already appears in the block on the device."
}
func (lineSubsetSuppress) MarkdownDescription(context.Context) string {
	return (lineSubsetSuppress{}).Description(nil)
}

func (lineSubsetSuppress) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return // create — nothing to reconcile against
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	// Prior state holds the device's current block lines (refreshed by Read). If
	// every declared (config) line already appears there, keep prior state and
	// show no diff; otherwise leave the config value so the drift is an update.
	if lineSubsetMatches(req.StateValue.ValueString(), req.ConfigValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// lineSubsetMatches reports whether every line in the config array is present
// (normalized) in the prior/live array (config is a subset of the live block).
// Invalid JSON on either side returns false so the caller falls back to a diff.
func lineSubsetMatches(stateLines, cfgLines string) bool {
	state, err := parseLines(stateLines)
	if err != nil {
		return false
	}
	cfg, err := parseLines(cfgLines)
	if err != nil {
		return false
	}
	set := normalizedSet(state)
	for _, l := range cfg {
		if _, ok := set[jetstream.NormalizeLine(l)]; !ok {
			return false
		}
	}
	return true
}
