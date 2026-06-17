// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider implements the jetstream OpenTofu/Terraform provider — a
// native client for TP-Link JetStream smart switches (e.g. TL-SG2008) over
// TELNET. The switch presents only a legacy ssh-dss host key (SSH unusable on
// modern clients) and has no REST API on this firmware, so its IOS-style telnet
// CLI is the transport. The provider is generic over the CLI: the
// jetstream_object resource/data source manage any running-config block
// (manage-declared-only), giving full coverage without per-feature code.
package provider

import (
	"context"
	"strconv"

	"github.com/JamesonRGrieve/tofu-jetstream/internal/jetstream"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*jetstreamProvider)(nil)

// New returns the provider factory for a given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &jetstreamProvider{version: version} }
}

type jetstreamProvider struct {
	version string
}

type providerModel struct {
	Host       types.String `tfsdk:"host"`
	Username   types.String `tfsdk:"username"`
	Password   types.String `tfsdk:"password"`
	TelnetPort types.Int64  `tfsdk:"telnet_port"`
}

func (p *jetstreamProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	// Single-token type name -> resources are `jetstream_object`, so Terraform's
	// prefix-before-first-underscore inference resolves the local name cleanly
	// (the source address is still jamesonrgrieve/jetstream).
	resp.TypeName = "jetstream"
	resp.Version = p.version
}

func (p *jetstreamProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Native provider for TP-Link JetStream smart switches (e.g. TL-SG2008) driven over " +
			"TELNET. SSH is unusable (the switch offers only a legacy ssh-dss host key) and there is no REST " +
			"API on this firmware, so the IOS-style telnet CLI is the transport. Configuration is expressed " +
			"generically as running-config blocks via the `jetstream_object` resource (manage-declared-only).",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Switch address (host or host:port), no scheme. The default telnet port is 23.",
			},
			"username": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Telnet login username (default `admin`).",
			},
			"password": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Telnet login password. Inject from the secret store via the provider block " +
					"(e.g. an ephemeral OpenBao read) — never hard-code it.",
			},
			"telnet_port": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Telnet port (default 23). A port embedded in `host` takes precedence.",
			},
		},
	}
}

func (p *jetstreamProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	port := ""
	if !cfg.TelnetPort.IsNull() && !cfg.TelnetPort.IsUnknown() && cfg.TelnetPort.ValueInt64() > 0 {
		port = strconv.FormatInt(cfg.TelnetPort.ValueInt64(), 10)
	}
	client := jetstream.NewClient(jetstream.Config{
		Host:     cfg.Host.ValueString(),
		Username: cfg.Username.ValueString(),
		Password: cfg.Password.ValueString(),
		Port:     port,
	})
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *jetstreamProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource, NewReconcileResource}
}

func (p *jetstreamProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}
