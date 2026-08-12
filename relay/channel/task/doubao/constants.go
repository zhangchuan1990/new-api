package doubao

import "strings"

var ModelList = []string{
	"doubao-seedance-1-0-pro-250528",
	"doubao-seedance-1-0-lite-t2v",
	"doubao-seedance-1-0-lite-i2v",
	"doubao-seedance-1-5-pro-251215",
	"doubao-seedance-2-0-260128",
	"doubao-seedance-2-0-fast-260128",
}

var ChannelName = "doubao-video"

// videoInputRatioMap 480P/720P 分辨率下视频输入折扣比率。
// 计算方式：含视频单价 / 720P不含视频单价。
// 管理员应将 ModelRatio 设置为 720P 不含视频的费率，
// 系统在检测到视频输入时自动乘以此折扣。
// 官网定价（元/百万token）：
//   doubao-seedance-2-0-260128:      720P含视频=28, 720P不含视频=46 → 28/46 ≈ 0.6087
//   doubao-seedance-2-0-fast-260128: 720P含视频=22, 720P不含视频=37 → 22/37 ≈ 0.5946
var videoInputRatioMap = map[string]float64{
	"doubao-seedance-2-0-260128":      28.0 / 46.0, // ~0.6087
	"doubao-seedance-2-0-fast-260128": 22.0 / 37.0, // ~0.5946
}

// videoInputRatio1080PMap 1080P 分辨率下视频输入折扣比率。
// 计算方式：1080P含视频单价 / 720P不含视频单价。
// 官网定价（元/百万token）：
//   doubao-seedance-2-0-260128: 1080P含视频=31, 720P不含视频=46 → 31/46 ≈ 0.6739
//   doubao-seedance-2-0-fast-260128: 不支持1080P，不在此映射中
var videoInputRatio1080PMap = map[string]float64{
	"doubao-seedance-2-0-260128": 31.0 / 46.0, // ~0.6739
}

// resolutionRatio1080PMap 1080P 分辨率加价比率。
// 计算方式：1080P不含视频单价 / 720P不含视频单价。
// 官网定价（元/百万token）：
//   doubao-seedance-2-0-260128: 1080P不含视频=51, 720P不含视频=46 → 51/46 ≈ 1.1087
//   doubao-seedance-2-0-fast-260128: 不支持1080P，不在此映射中
var resolutionRatio1080PMap = map[string]float64{
	"doubao-seedance-2-0-260128": 51.0 / 46.0, // ~1.1087
}

// GetVideoInputRatio 返回指定模型在 480P/720P 分辨率下的视频输入折扣比率。
func GetVideoInputRatio(modelName string) (float64, bool) {
	r, ok := videoInputRatioMap[modelName]
	return r, ok
}

// GetVideoInputRatio1080P 返回指定模型在 1080P 分辨率下的视频输入折扣比率。
func GetVideoInputRatio1080P(modelName string) (float64, bool) {
	r, ok := videoInputRatio1080PMap[modelName]
	return r, ok
}

// GetResolutionRatio1080P 返回指定模型的 1080P 分辨率加价比率（相对 720P 不含视频基准价）。
func GetResolutionRatio1080P(modelName string) (float64, bool) {
	r, ok := resolutionRatio1080PMap[modelName]
	return r, ok
}

// Is1080P 判断分辨率字符串是否表示 1080P。
// 支持大小写及纯数字格式：1080p, 1080P, 1080。
func Is1080P(resolution string) bool {
	switch resolution {
	case "1080p", "1080P", "1080":
		return true
	}
	return false
}
