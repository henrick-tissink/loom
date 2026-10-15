package main

import "runtime/debug"

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	v := info.Main.Version
	if v == "(devel)" || v == "" {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 8 {
				return s.Value[:8]
			}
		}
		return ""
	}
	return v
}
