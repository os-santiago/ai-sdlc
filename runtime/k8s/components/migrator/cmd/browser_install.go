package main

import (
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"io"
	"time"
)

func compileBrowserInstallation(input io.Reader, output io.Writer) error {
	denied := errors.New("browser installation input denied")
	raw, e := io.ReadAll(io.LimitReader(input, 512*1024+1))
	var req podtransport.BrowserInstallation
	fields := []string{"target", "profile", "cluster", "namespace", "node", "runtimeClass", "ca", "policies", "consoleIngress"}
	if e != nil || len(raw) > 512*1024 || !exactInstallFields(raw, fields, nil) || authority.DecodeStrict(raw, &req) != nil || req.Validate(time.Now()) != nil {
		return denied
	}
	return json.NewEncoder(output).Encode(struct {
		Version       string               `json:"version"`
		Profile       browsercheck.Profile `json:"profile"`
		ProfileSHA256 string               `json:"profileSHA256"`
		TargetSHA256  string               `json:"targetSHA256"`
	}{"ai-sdlc.browser-installation/v1", req.Profile, req.Profile.Digest(), req.Target.Digest()})
}
