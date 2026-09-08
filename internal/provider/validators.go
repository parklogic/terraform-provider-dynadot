package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

var hostnameLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

func validHostname(value string) bool {
	if value != strings.TrimSpace(value) {
		return false
	}
	value = strings.TrimSuffix(value, ".")
	if len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) > 63 || !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

type hostnameValidator struct{}

func (hostnameValidator) Description(context.Context) string {
	return "A valid fully qualified nameserver hostname."
}
func (v hostnameValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (hostnameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if !req.ConfigValue.IsNull() && !req.ConfigValue.IsUnknown() && !validHostname(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid hostname", "Use a fully qualified hostname with valid DNS labels.")
	}
}

type ipSetValidator struct{}

func (ipSetValidator) Description(context.Context) string {
	return "A nonempty set of valid IPv4 or IPv6 addresses."
}
func (v ipSetValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (ipSetValidator) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if len(req.ConfigValue.Elements()) == 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Empty IP addresses", "At least one IP address is required.")
	}
	for _, element := range req.ConfigValue.Elements() {
		v, ok := element.(types.String)
		if !ok || v.IsNull() {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid IP address", "IP addresses cannot contain null elements.")
			continue
		}
		if v.IsUnknown() {
			continue
		}
		if _, err := dynadot.NormalizeIPs([]string{v.ValueString()}); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid IP address", err.Error())
		}
	}
}

type durationValidator struct{}

func (durationValidator) Description(context.Context) string {
	return "A positive Go duration, such as 30s or 5m."
}
func (v durationValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := parseTimeout(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid timeout", err.Error())
	}
}
func parseTimeout(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("timeout must be a positive duration, such as 30s or 5m")
	}
	return d, nil
}
