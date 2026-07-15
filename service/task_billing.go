package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

// LogTaskConsumption 记录任务消费日志和统计信息（仅记录，不涉及实际扣费）。
// 实际扣费已由 BillingSession（PreConsumeBilling + SettleBilling）完成。
//
// 日志格式说明：
//   - 计费参数（无前缀 key）：实际参与扣费的倍率值
//   - 诊断信息（_ 前缀 key）：辅助核对金额的详细信息，不参与计费
//     _vidu_xxx 系列来自 Vidu adaptor 的 EstimateBilling 返回值
func LogTaskConsumption(c *gin.Context, info *relaycommon.RelayInfo) {
	tokenName := c.GetString("token_name")
	logContent := fmt.Sprintf("操作 %s", info.Action)
	// 支持任务仅按次计费
	if common.StringsContains(constant.TaskPricePatches, info.OriginModelName) {
		logContent = fmt.Sprintf("%s，按次计费", logContent)
	} else if len(info.PriceData.OtherRatios) > 0 {
		var billingContents []string
		var diagContents []string
		for key, ra := range info.PriceData.OtherRatios {
			if strings.HasPrefix(key, "_") {
				// 诊断信息：转换为可读格式
				diagContents = append(diagContents, formatDiagKey(key, ra))
			} else if ra != 1.0 {
				// 计费参数：实际的费用倍率
				billingContents = append(billingContents, fmt.Sprintf("%s: %.2f", key, ra))
			}
		}
		if len(billingContents) > 0 {
			logContent = fmt.Sprintf("%s, 计算参数：%s", logContent, strings.Join(billingContents, ", "))
		}
		if len(diagContents) > 0 {
			logContent = fmt.Sprintf("%s, [%s]", logContent, strings.Join(diagContents, ", "))
		}
	}
	other := make(map[string]interface{})
	other["is_task"] = true
	other["request_path"] = c.Request.URL.Path
	other["model_price"] = info.PriceData.ModelPrice
	if info.PriceData.ModelRatio > 0 {
		other["model_ratio"] = info.PriceData.ModelRatio
	}
	other["group_ratio"] = info.PriceData.GroupRatioInfo.GroupRatio
	if info.PriceData.GroupRatioInfo.HasSpecialRatio {
		other["user_group_ratio"] = info.PriceData.GroupRatioInfo.GroupSpecialRatio
	}
	if info.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = info.UpstreamModelName
	}
	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId: info.ChannelId,
		ModelName: info.OriginModelName,
		TokenName: tokenName,
		Quota:     info.PriceData.Quota,
		Content:   logContent,
		TokenId:   info.TokenId,
		Group:     info.UsingGroup,
		Other:     other,
	})
	model.UpdateUserUsedQuotaAndRequestCount(info.UserId, info.PriceData.Quota)
	model.UpdateChannelUsedQuota(info.ChannelId, info.PriceData.Quota)
}

// ---------------------------------------------------------------------------
// 异步任务计费辅助函数
// ---------------------------------------------------------------------------

// resolveTokenKey 通过 TokenId 运行时获取令牌 Key（用于 Redis 缓存操作）。
// 如果令牌已被删除或查询失败，返回空字符串。
func resolveTokenKey(ctx context.Context, tokenId int, taskID string) string {
	token, err := model.GetTokenById(tokenId)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("获取令牌 key 失败 (tokenId=%d, task=%s): %s", tokenId, taskID, err.Error()))
		return ""
	}
	return token.Key
}

// taskIsSubscription 判断任务是否通过订阅计费。
func taskIsSubscription(task *model.Task) bool {
	return task.PrivateData.BillingSource == BillingSourceSubscription && task.PrivateData.SubscriptionId > 0
}

// taskAdjustFunding 调整任务的资金来源（钱包或订阅），delta > 0 表示扣费，delta < 0 表示退还。
func taskAdjustFunding(task *model.Task, delta int) error {
	if taskIsSubscription(task) {
		return model.PostConsumeUserSubscriptionDelta(task.PrivateData.SubscriptionId, int64(delta))
	}
	if delta > 0 {
		return model.DecreaseUserQuota(task.UserId, delta, false)
	}
	return model.IncreaseUserQuota(task.UserId, -delta, false)
}

// taskAdjustTokenQuota 调整任务的令牌额度，delta > 0 表示扣费，delta < 0 表示退还。
// 需要通过 resolveTokenKey 运行时获取 key（不从 PrivateData 中读取）。
func taskAdjustTokenQuota(ctx context.Context, task *model.Task, delta int) {
	if task.PrivateData.TokenId <= 0 || delta == 0 {
		return
	}
	tokenKey := resolveTokenKey(ctx, task.PrivateData.TokenId, task.TaskID)
	if tokenKey == "" {
		return
	}
	var err error
	if delta > 0 {
		err = model.DecreaseTokenQuota(task.PrivateData.TokenId, tokenKey, delta)
	} else {
		err = model.IncreaseTokenQuota(task.PrivateData.TokenId, tokenKey, -delta)
	}
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("调整令牌额度失败 (delta=%d, task=%s): %s", delta, task.TaskID, err.Error()))
	}
}

// taskBillingOther 从 task 的 BillingContext 构建日志 Other 字段。
func taskBillingOther(task *model.Task) map[string]interface{} {
	other := make(map[string]interface{})
	if bc := task.PrivateData.BillingContext; bc != nil {
		other["model_price"] = bc.ModelPrice
		if bc.ModelRatio > 0 {
			other["model_ratio"] = bc.ModelRatio
		}
		other["group_ratio"] = bc.GroupRatio
		if len(bc.OtherRatios) > 0 {
			for k, v := range bc.OtherRatios {
				other[k] = v
			}
		}
	}
	props := task.Properties
	if props.UpstreamModelName != "" && props.UpstreamModelName != props.OriginModelName {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = props.UpstreamModelName
	}
	return other
}

// taskModelName 从 BillingContext 或 Properties 中获取模型名称。
func taskModelName(task *model.Task) string {
	if bc := task.PrivateData.BillingContext; bc != nil && bc.OriginModelName != "" {
		return bc.OriginModelName
	}
	return task.Properties.OriginModelName
}

// RefundTaskQuota 统一的任务失败退款逻辑。
// 当异步任务失败时，将预扣的 quota 退还给用户（支持钱包和订阅），并退还令牌额度。
func RefundTaskQuota(ctx context.Context, task *model.Task, reason string) {
	quota := task.Quota
	if quota == 0 {
		return
	}

	// 1. 退还资金来源（钱包或订阅）
	if err := taskAdjustFunding(task, -quota); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("退还资金来源失败 task %s: %s", task.TaskID, err.Error()))
		return
	}

	// 2. 退还令牌额度
	taskAdjustTokenQuota(ctx, task, -quota)

	// 3. 记录日志
	other := taskBillingOther(task)
	other["task_id"] = task.TaskID
	other["reason"] = reason
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeRefund,
		Content:   "",
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     quota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
	})
}

// RecalculateTaskQuota 通用的异步差额结算。
// actualQuota 是任务完成后的实际应扣额度，与预扣额度 (task.Quota) 做差额结算。
// reason 用于日志记录（例如 "token重算" 或 "adaptor调整"）。
func RecalculateTaskQuota(ctx context.Context, task *model.Task, actualQuota int, reason string) {
	if actualQuota <= 0 {
		return
	}
	preConsumedQuota := task.Quota
	quotaDelta := actualQuota - preConsumedQuota

	if quotaDelta == 0 {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 预扣费准确（%s，%s）",
			task.TaskID, logger.LogQuota(actualQuota), reason))
		return
	}

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 差额结算：delta=%s（实际：%s，预扣：%s，%s）",
		task.TaskID,
		logger.LogQuota(quotaDelta),
		logger.LogQuota(actualQuota),
		logger.LogQuota(preConsumedQuota),
		reason,
	))

	// 调整资金来源
	if err := taskAdjustFunding(task, quotaDelta); err != nil {
		logger.LogError(ctx, fmt.Sprintf("差额结算资金调整失败 task %s: %s", task.TaskID, err.Error()))
		return
	}

	// 调整令牌额度
	taskAdjustTokenQuota(ctx, task, quotaDelta)

	task.Quota = actualQuota

	var logType int
	var logQuota int
	if quotaDelta > 0 {
		logType = model.LogTypeConsume
		logQuota = quotaDelta
		model.UpdateUserUsedQuotaAndRequestCount(task.UserId, quotaDelta)
		model.UpdateChannelUsedQuota(task.ChannelId, quotaDelta)
	} else {
		logType = model.LogTypeRefund
		logQuota = -quotaDelta
	}
	other := taskBillingOther(task)
	other["task_id"] = task.TaskID
	other["pre_consumed_quota"] = preConsumedQuota
	other["actual_quota"] = actualQuota
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   logType,
		Content:   reason,
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     logQuota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
	})
}

// RecalculateTaskQuotaByTokens 根据实际 token 消耗重新计费（异步差额结算）。
// 当任务成功且返回了 totalTokens 时，根据模型倍率和分组倍率重新计算实际扣费额度，
// 与预扣费的差额进行补扣或退还。支持钱包和订阅计费来源。
func RecalculateTaskQuotaByTokens(ctx context.Context, task *model.Task, totalTokens int) {
	if totalTokens <= 0 {
		return
	}

	modelName := taskModelName(task)

	// 获取模型价格和倍率
	modelRatio, hasRatioSetting, _ := ratio_setting.GetModelRatio(modelName)
	// 只有配置了倍率(非固定价格)时才按 token 重新计费
	if !hasRatioSetting || modelRatio <= 0 {
		return
	}

	// 获取用户和组的倍率信息
	group := task.Group
	if group == "" {
		user, err := model.GetUserById(task.UserId, false)
		if err == nil {
			group = user.Group
		}
	}
	if group == "" {
		return
	}

	groupRatio := ratio_setting.GetGroupRatio(group)
	userGroupRatio, hasUserGroupRatio := ratio_setting.GetGroupGroupRatio(group, group)

	var finalGroupRatio float64
	if hasUserGroupRatio {
		finalGroupRatio = userGroupRatio
	} else {
		finalGroupRatio = groupRatio
	}

	// 计算 OtherRatios 乘积（视频折扣、时长等）
	otherMultiplier := 1.0
	if bc := task.PrivateData.BillingContext; bc != nil {
		for _, r := range bc.OtherRatios {
			if r != 1.0 && r > 0 {
				otherMultiplier *= r
			}
		}
	}

	// 计算实际应扣费额度: totalTokens * modelRatio * groupRatio * otherMultiplier
	actualQuota := int(float64(totalTokens) * modelRatio * finalGroupRatio * otherMultiplier)

	reason := fmt.Sprintf("token重算：tokens=%d, modelRatio=%.2f, groupRatio=%.2f, otherMultiplier=%.4f", totalTokens, modelRatio, finalGroupRatio, otherMultiplier)
	RecalculateTaskQuota(ctx, task, actualQuota, reason)
}

// ---------------------------------------------------------------------------
// 诊断信息格式化（用于 _ 前缀的 OtherRatios key）
// ---------------------------------------------------------------------------

// formatDiagKey 将 _ 前缀的诊断 key 转换为可读的日志字符串。
//
// 支持的 Vidu 诊断 key：
//   _vidu_seconds      → "时长: 4s"
//   _vidu_resolution   → "清晰度: 720p" (编码: 1=360p, 2=540p, 3=720p, 4=1080p)
//   _vidu_action      → "动作: 图生" (编码: 1=图生/文生, 2=首尾帧, 3=参考生)
//   _vidu_cps         → "单价: 20积分/s"
//   _vidu_total_credits → "积分: 80"
//   _vidu_cost_rmb    → "费用: ¥2.50"
//
// 支持的 Doubao（火山方舟）诊断 key：
//   _doubao_seconds    → "时长: 5s"
//   _doubao_resolution → "分辨率: 720p" (编码: 2=720p, 4=1080p)
//   _doubao_has_video  → "视频输入: 是/否" (0=否, 1=是)
//   _doubao_ratio_type → "倍率类型: 基准价/视频折扣(720p)/加价(1080p)/视频折扣(1080p)"
//   _doubao_ratio_value → "倍率值: 0.61"
//   _doubao_base_price → "官网基准: ¥46/1M"
//
// 其他 _ 前缀 key 以原始格式输出（"key: value"）
func formatDiagKey(key string, value float64) string {
	switch key {
	// ===== Vidu 诊断信息 =====
	case "_vidu_seconds":
		return fmt.Sprintf("时长: %.0fs", value)
	case "_vidu_resolution":
		return fmt.Sprintf("清晰度: %s", resolutionCodeToString(int(value)))
	case "_vidu_action":
		return fmt.Sprintf("动作: %s", actionCodeToString(int(value)))
	case "_vidu_cps":
		return fmt.Sprintf("单价: %.0f积分/s", value)
	case "_vidu_total_credits":
		return fmt.Sprintf("积分: %.0f", value)
	case "_vidu_cost_rmb":
		// 使用 4 位小数，避免与系统实际扣费金额因舍入不一致
		return fmt.Sprintf("费用: ¥%.4f", value)

	// ===== Doubao（火山方舟）诊断信息 =====
	case "_doubao_seconds":
		return fmt.Sprintf("时长: %.0fs", value)
	case "_doubao_resolution":
		return fmt.Sprintf("分辨率: %s", doubaoResolutionCodeToString(int(value)))
	case "_doubao_has_video":
		if value > 0 {
			return "视频输入: 是"
		}
		return "视频输入: 否"
	case "_doubao_ratio_type":
		return fmt.Sprintf("倍率类型: %s", doubaoRatioTypeToString(int(value)))
	case "_doubao_ratio_value":
		return fmt.Sprintf("倍率值: %.4f", value)
	case "_doubao_base_price":
		if value > 0 {
			return fmt.Sprintf("官网基准: ¥%.0f/1M", value)
		}
		return "官网基准: 未知"

	default:
		// 未知的诊断 key，保留原始格式
		return fmt.Sprintf("%s: %.2f", strings.TrimPrefix(key, "_"), value)
	}
}

// resolutionCodeToString 将分辨率数值编码转换为可读字符串。
func resolutionCodeToString(code int) string {
	switch code {
	case 1:
		return "360p"
	case 2:
		return "540p"
	case 3:
		return "720p"
	case 4:
		return "1080p"
	default:
		return fmt.Sprintf("未知(%d)", code)
	}
}

// actionCodeToString 将动作类型数值编码转换为可读字符串。
func actionCodeToString(code int) string {
	switch code {
	case 1:
		return "图生"
	case 2:
		return "首尾帧"
	case 3:
		return "参考生"
	default:
		return fmt.Sprintf("未知(%d)", code)
	}
}

// ---------------------------------------------------------------------------
// Doubao（火山方舟）诊断信息辅助函数
// ---------------------------------------------------------------------------

// doubaoResolutionCodeToString 将 Doubao 分辨率编码转换为可读字符串。
// 编码规则：2 = 720p, 4 = 1080p
func doubaoResolutionCodeToString(code int) string {
	switch code {
	case 2:
		return "720p"
	case 4:
		return "1080p"
	default:
		return fmt.Sprintf("未知(%d)", code)
	}
}

// doubaoRatioTypeToString 将 Doubao 倍率类型编码转换为可读字符串。
//
// 编码规则：
//   0 = 基准价（720P 不含视频，无额外倍率）
//   1 = 视频输入折扣（720P 含视频，相对于基准价的折扣）
//   2 = 分辨率加价（1080P 不含视频，相对于基准价的加价）
//   3 = 视频输入折扣（1080P 含视频，已包含分辨率折扣）
func doubaoRatioTypeToString(code int) string {
	switch code {
	case 0:
		return "基准价(720p不含视频)"
	case 1:
		return "视频折扣(720p)"
	case 2:
		return "加价(1080p)"
	case 3:
		return "视频折扣(1080p)"
	default:
		return fmt.Sprintf("未知(%d)", code)
	}
}
