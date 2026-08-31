package configfile

func (i *ProxiedServerClientInfo) ResolvedLogPath(beadsDir string) string {
	if i == nil {
		return ""
	}
	return resolveSidecarPath(beadsDir, i.LogPath)
}
