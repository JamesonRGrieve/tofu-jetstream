// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"

	"github.com/JamesonRGrieve/tofu-jetstream/internal/jetstream"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource              = (*reconcileResource)(nil)
	_ resource.ResourceWithConfigure = (*reconcileResource)(nil)
)

// NewReconcileResource constructs the jetstream_reconcile resource: an
// unconditional config re-apply + save on every run. It manages no remote object
// — it exists to heal config-vs-live drift Terraform cannot detect. The provider
// tracks the running-config blocks it reads back, so a plan with 0 object changes
// never re-applies; an out-of-band edit (or a startup-config that diverged from
// running) therefore goes unhealed. This resource re-runs a declared list of
// config-mode `commands` (entered under `configure`, then `copy running-config
// startup-config`) unconditionally. Pair with a `triggers` map holding
// `timestamp()` to fire every run. The commands are verbatim CLI lines and may
// switch context themselves (e.g. `interface gigabitEthernet 1/0/6` followed by
// its body lines).
func NewReconcileResource() resource.Resource { return &reconcileResource{} }

type reconcileResource struct {
	client *jetstream.Client
}

type reconcileModel struct {
	ID       types.String `tfsdk:"id"`
	Commands types.List   `tfsdk:"commands"`
	Triggers types.Map    `tfsdk:"triggers"`
}

func (r *reconcileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reconcile"
}

func (r *reconcileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Unconditional reconcile. Runs each line in `commands` under `configure` and then " +
			"`copy running-config startup-config` on every create/update — it manages no remote object. Pair " +
			"with a `triggers` map containing `timestamp()` so it re-runs every run, healing config-vs-live " +
			"drift Terraform cannot detect (the provider tracks the running-config it reads, so a 0-change plan " +
			"otherwise never re-applies an out-of-band edit, nor re-saves a diverged startup-config). Commands " +
			"are verbatim CLI lines and may switch context themselves.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Static resource id (`reconcile`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"commands": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Ordered list of config-mode CLI lines to re-apply (then save) on every run.",
			},
			"triggers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Arbitrary key/value map; any change re-runs the reconcile. Set a key to `timestamp()` to fire every run.",
			},
		},
	}
}

func (r *reconcileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *reconcileResource) reconcile(ctx context.Context, m reconcileModel, diags *diag.Diagnostics) {
	var cmds []string
	diags.Append(m.Commands.ElementsAs(ctx, &cmds, false)...)
	if diags.HasError() {
		return
	}
	// ApplyLines with an empty context enters `configure`, runs each command
	// verbatim, exits, then saves — exactly the unconditional reconcile.
	if err := r.client.ApplyLines(ctx, "", cmds); err != nil {
		diags.AddError("JetStream reconcile failed", err.Error())
	}
}

func (r *reconcileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.reconcile(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// No remote object to read; keep prior state verbatim.
	var m reconcileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.reconcile(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Manages no remote object — nothing to delete.
}
