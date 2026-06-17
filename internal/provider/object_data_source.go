// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"

	"github.com/JamesonRGrieve/tofu-jetstream/internal/jetstream"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*objectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*objectDataSource)(nil)
)

// NewObjectDataSource constructs the jetstream_object data source.
func NewObjectDataSource() datasource.DataSource { return &objectDataSource{} }

type objectDataSource struct {
	client *jetstream.Client
}

type objectDataModel struct {
	Context types.String `tfsdk:"context"`
	Lines   types.String `tfsdk:"lines"`
	Present types.Bool   `tfsdk:"present"`
	All     types.String `tfsdk:"all"`
}

func (d *objectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (d *objectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Read JetStream running-config. Set `context` to read one block's lines " +
			"(`lines` JSON array + `present`); omit `context` to read the whole `show running-config` into `all`.",
		Attributes: map[string]schema.Attribute{
			"context": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Config block to read (e.g. `vlan 2`, `interface gigabitEthernet 1/0/6`, " +
					"empty string for global). Omit entirely to read the whole config into `all`.",
			},
			"lines": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "JSON array of the block's lines (empty array when reading the whole config).",
			},
			"present": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the block exists on the device (false when `context` is omitted).",
			},
			"all": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Full `show running-config` output when `context` is omitted; empty otherwise.",
			},
		},
	}
}

func (d *objectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*jetstream.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *jetstream.Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *objectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m objectDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.Context.IsNull() {
		raw, err := d.client.RunningConfig(ctx)
		if err != nil {
			resp.Diagnostics.AddError("JetStream show running-config failed", err.Error())
			return
		}
		m.All = types.StringValue(raw)
		m.Lines = types.StringValue("[]")
		m.Present = types.BoolValue(false)
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	lines, present, err := d.client.Block(ctx, m.Context.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("JetStream read block failed", err.Error())
		return
	}
	m.Lines = types.StringValue(marshalLines(lines))
	m.Present = types.BoolValue(present)
	m.All = types.StringValue("")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
