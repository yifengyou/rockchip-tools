package ws

import (
	_ "embed"
	"mime"
	"path"
)

// 内嵌前端资源：单文件可执行，无需外部依赖。
var (
	//go:embed dashboard.html
	dashboardHTML string

	//go:embed static/xterm.js
	assetXtermJS []byte

	//go:embed static/xterm.css
	assetXtermCSS []byte

	//go:embed static/addon-fit.js
	assetAddonFit []byte

	//go:embed static/addon-web-links.js
	assetAddonLinks []byte
)

// staticAsset 按键名返回静态资源与 Content-Type。
func staticAsset(name string) ([]byte, string, bool) {
	var data []byte
	switch name {
	case "xterm.js":
		data = assetXtermJS
	case "xterm.css":
		data = assetXtermCSS
	case "addon-fit.js":
		data = assetAddonFit
	case "addon-web-links.js":
		data = assetAddonLinks
	default:
		return nil, "", false
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		if path.Ext(name) == ".css" {
			ctype = "text/css; charset=utf-8"
		} else {
			ctype = "application/javascript; charset=utf-8"
		}
	}
	return data, ctype, true
}
