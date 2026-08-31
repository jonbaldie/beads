package configfile

func (i *ProxiedServerClientInfo) ResolvedRootPath(beadsDir string) string {
	if i == nil {
		return ""
	}
	return resolveSidecarPath(beadsDir, i.RootPath)
}
