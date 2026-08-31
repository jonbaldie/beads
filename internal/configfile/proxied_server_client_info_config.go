package configfile

func (i *ProxiedServerClientInfo) ResolvedConfigPath(beadsDir string) string {
	if i == nil {
		return ""
	}
	return resolveSidecarPath(beadsDir, i.ConfigPath)
}
