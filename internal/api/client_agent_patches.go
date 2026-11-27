package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
)

// GetAgentPatch reads the raw patch and its CAS revision. A missing patch
// returns revision "absent" so a caller can conditionally create it.
func (c *Client) GetAgentPatch(name string) (configedit.AgentPatchMergeResult, error) {
	if err := c.requireCityScope(); err != nil {
		return configedit.AgentPatchMergeResult{}, err
	}
	decode := func(status int, pd *genclient.ErrorModel, headers http.Header, body []byte) (configedit.AgentPatchMergeResult, error) {
		if status == http.StatusNotFound && pd != nil && pd.Code != nil && *pd.Code == "patch-not-found" {
			return configedit.AgentPatchMergeResult{Revision: configedit.AgentPatchRevision(nil)}, nil
		}
		if err := apiErrorFromResponse(status, pd); err != nil {
			return configedit.AgentPatchMergeResult{}, err
		}
		var patch config.AgentPatch
		if err := json.Unmarshal(body, &patch); err != nil {
			return configedit.AgentPatchMergeResult{}, fmt.Errorf("decoding agent patch: %w", err)
		}
		revision := headers.Get("X-GC-Patch-Revision")
		if revision == "" {
			return configedit.AgentPatchMergeResult{}, fmt.Errorf("API did not return an agent patch revision; upgrade the server")
		}
		return configedit.AgentPatchMergeResult{After: &patch, Revision: revision}, nil
	}
	if dir, base, qualified := strings.Cut(name, "/"); qualified {
		resp, err := c.cw.GetV0CityByCityNamePatchesAgentByDirByBaseWithResponse(context.Background(), c.cityName, dir, base)
		if err != nil {
			return configedit.AgentPatchMergeResult{}, &connError{err: err}
		}
		return decode(resp.StatusCode(), pdOf(resp), resp.HTTPResponse.Header, resp.Body)
	}
	resp, err := c.cw.GetV0CityByCityNamePatchesAgentByBaseWithResponse(context.Background(), c.cityName, name)
	if err != nil {
		return configedit.AgentPatchMergeResult{}, &connError{err: err}
	}
	return decode(resp.StatusCode(), pdOf(resp), resp.HTTPResponse.Header, resp.Body)
}

// MergeAgentPatch performs the same revision-guarded edit the HTTP patch
// resource exposes. Errors never trigger a local-file retry of this mutation.
func (c *Client) MergeAgentPatch(change configedit.AgentPatchMerge) (configedit.AgentPatchMergeResult, error) {
	if err := c.requireCityScope(); err != nil {
		return configedit.AgentPatchMergeResult{}, err
	}
	body := genclient.AgentPatchMerge{
		Name: change.Name, Revision: change.Revision, Provider: change.Provider,
		ClearProvider: &change.ClearProvider, Delete: &change.Delete,
	}
	if change.Dir != "" {
		body.Dir = &change.Dir
	}
	if change.Rig != "" {
		body.Rig = &change.Rig
	}
	if change.OptionDefaults != nil {
		body.OptionDefaults = &change.OptionDefaults
	}
	if change.RemoveOptions != nil {
		body.RemoveOptions = &change.RemoveOptions
	}
	resp, err := c.cw.PatchV0CityByCityNamePatchesAgentsWithResponse(context.Background(), c.cityName,
		&genclient.PatchV0CityByCityNamePatchesAgentsParams{XGCRequest: "true"}, body)
	if err != nil {
		return configedit.AgentPatchMergeResult{}, &connError{err: err}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return configedit.AgentPatchMergeResult{}, err
	}
	var result configedit.AgentPatchMergeResult
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return result, fmt.Errorf("decoding agent patch merge: %w", err)
	}
	return result, nil
}
