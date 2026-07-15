package vidu

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/pkg/errors"
)

// ============================
// Request / Response structures
// ============================

type requestPayload struct {
	Model             string   `json:"model"`
	Images            []string `json:"images"`
	Prompt            string   `json:"prompt,omitempty"`
	Duration          int      `json:"duration,omitempty"`
	Seed              int      `json:"seed,omitempty"`
	Resolution        string   `json:"resolution,omitempty"`
	MovementAmplitude string   `json:"movement_amplitude,omitempty"`
	Bgm               bool     `json:"bgm,omitempty"`
	Payload           string   `json:"payload,omitempty"`
	CallbackUrl       string   `json:"callback_url,omitempty"`
}

type responsePayload struct {
	TaskId            string   `json:"task_id"`
	State             string   `json:"state"`
	Model             string   `json:"model"`
	Images            []string `json:"images"`
	Prompt            string   `json:"prompt"`
	Duration          int      `json:"duration"`
	Seed              int      `json:"seed"`
	Resolution        string   `json:"resolution"`
	Bgm               bool     `json:"bgm"`
	MovementAmplitude string   `json:"movement_amplitude"`
	Payload           string   `json:"payload"`
	CreatedAt         string   `json:"created_at"`
}

type taskResultResponse struct {
	State     string     `json:"state"`
	ErrCode   string     `json:"err_code"`
	Credits   int        `json:"credits"`
	Payload   string     `json:"payload"`
	Creations []creation `json:"creations"`
}

type creation struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	CoverURL string `json:"cover_url"`
}

// ============================
// Adaptor implementation
// ============================

// TaskAdaptor 实现 Vidu 视频生成渠道的适配器，支持 Q1/Q2/Q3 全系列模型。
//
// 计费说明（与官网保持一致）：
//   - Vidu 官方采用积分制计费：1 积分 = ¥0.03125
//   - 不同模型、分辨率、动作类型、时长的积分消耗不同
//   - 本适配器通过 EstimateBilling() 动态计算每次请求的实际费用
//   - 前端需为每个模型配置 ModelRatio（表示 720p 分辨率下标准动作的每秒积分换算值）
type TaskAdaptor struct {
	// 不再嵌入 BaseBilling，改为自定义实现 EstimateBilling 以支持动态计费
	ChannelType int
	baseURL     string
}

// ---------------------------------------------------------------------------
// Vidu 官方定价常量（来自 https://platform.vidu.cn/pricing）
// ---------------------------------------------------------------------------
//
// 定价体系说明：
//   - 积分单价：1 积分 = ¥0.03125 RMB
//   - 系统汇率设置：用户已将 USD2RMB 设为 1（即 1 美元 = 1 人民币）
//   - creditsPerRUnit = 0.0625（推导见常量定义处的详细注释）
//
// 下表中的数值均为「每秒积分消耗」，来源于官方文档。
// ---------------------------------------------------------------------------

const (
	// creditsPerRUnit 表示 EstimateBilling 返回的 multiplier 中，
	// 每 1 积分对应的倍率值。
	//
	// 推导过程（USD2RMB=1 时）：
	//   系统计费公式：FinalQuota = (ModelRatio/2 * QuotaPerUnit) * vidu_billing
	//   最终费用(元) = FinalQuota / QuotaPerUnit = (ModelRatio/2) * vidu_billing
	//
	//   官方定价：总费用 = 总积分 * ¥0.03125
	//
	//   当 ModelRatio=1 时：
	//     (1/2) * vidu_billing = 总积分 * 0.03125
	//     vidu_billing = 总积分 * 0.0625
	//
	//   因此 creditsPerRUnit = 0.0625
	//   即：multiplier = totalCredits * creditsPerRUnit
	creditsPerRUnit = 0.0625

	// creditPriceRMB 官方积分单价：1 积分 = ¥0.03125 RMB（来自 Vidu 官方定价文档）
	creditPriceRMB = 0.03125
)

// viduPricingEntry 定义单个模型在特定动作和分辨率下的每秒积分消耗。
type viduPricingEntry struct {
	// modelKey 用于匹配请求中的模型名称（支持前缀匹配）
	modelKey string
	// actionType 对应的动作类型：generate(图生/文生)、first_tail(首尾帧)、reference(参考生)
	actionType string
	// resolution 分辨率："540p" / "720p" / "1080p"
	resolution string
	// creditsPerSecond 每秒消耗的积分（来自官方定价表）
	creditsPerSecond int
}

// viduPricingTable 是完整的 Vidu 定价表。
// 数据来源：Vidu 官方产品定价文档 (vidu 产品定价.md)
//
// 使用方式：
//   通过 lookupViduCredits(model, action, resolution) 查询每秒积分消耗，
//   再乘以实际秒数得到总积分，最后转换为系统 quota 单位。
var viduPricingTable = []viduPricingEntry{
	// ===== ViduQ3 系列（最新，支持 1-16 秒） =====
	// 文档位置：vidu 产品定价.md 第 33-57 行
	// 格式：model | action | resolution | 每秒积分

	// --- viduq3-pro: 图生视频 / 文生视频 / 首尾帧 ---
	{modelKey: "viduq3-pro", actionType: "generate", resolution: "1080p", creditsPerSecond: 24},
	{modelKey: "viduq3-pro", actionType: "generate", resolution: "720p", creditsPerSecond: 20},
	{modelKey: "viduq3-pro", actionType: "generate", resolution: "540p", creditsPerSecond: 9},
	{modelKey: "viduq3-pro", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 24},
	{modelKey: "viduq3-pro", actionType: "first_tail", resolution: "720p", creditsPerSecond: 20},
	{modelKey: "viduq3-pro", actionType: "first_tail", resolution: "540p", creditsPerSecond: 9},

	// --- viduq3-turbo: 图生视频 / 文生视频 / 首尾帧 ---
	{modelKey: "viduq3-turbo", actionType: "generate", resolution: "1080p", creditsPerSecond: 13},
	{modelKey: "viduq3-turbo", actionType: "generate", resolution: "720p", creditsPerSecond: 12},
	{modelKey: "viduq3-turbo", actionType: "generate", resolution: "540p", creditsPerSecond: 7},
	{modelKey: "viduq3-turbo", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 13},
	{modelKey: "viduq3-turbo", actionType: "first_tail", resolution: "720p", creditsPerSecond: 12},
	{modelKey: "viduq3-turbo", actionType: "first_tail", resolution: "540p", creditsPerSecond: 7},

	// --- viduq3-pro-fast: 仅支持图生视频 ---
	{modelKey: "viduq3-pro-fast", actionType: "generate", resolution: "1080p", creditsPerSecond: 15},
	{modelKey: "viduq3-pro-fast", actionType: "generate", resolution: "720p", creditsPerSecond: 12},

	// --- viduq3 参考生视频（标准版） ---
	{modelKey: "viduq3", actionType: "reference", resolution: "1080p", creditsPerSecond: 15},
	{modelKey: "viduq3", actionType: "reference", resolution: "720p", creditsPerSecond: 12},
	{modelKey: "viduq3", actionType: "reference", resolution: "540p", creditsPerSecond: 7},

	// --- viduq3-mix 参考生视频 ---
	{modelKey: "viduq3-mix", actionType: "reference", resolution: "1080p", creditsPerSecond: 29},
	{modelKey: "viduq3-mix", actionType: "reference", resolution: "720p", creditsPerSecond: 24},

	// --- viduq3-turbo 参考生视频 ---
	{modelKey: "viduq3-turbo", actionType: "reference", resolution: "1080p", creditsPerSecond: 13},
	{modelKey: "viduq3-turbo", actionType: "reference", resolution: "720p", creditsPerSecond: 10},
	{modelKey: "viduq3-turbo", actionType: "reference", resolution: "540p", creditsPerSecond: 4},

	// ===== ViduQ2 系列（按秒递增定价） =====
	// 文档位置：vidu 产品定价.md 第 59-85 行
	// 注意：Q2 采用「首秒 + 后续每秒」的递增模式，此处取 4s 平均值作为近似

	// --- Q2-turbo: 图生 & 首尾帧 ---
	// 720p: 第1秒8 + 第2秒10 + 第3秒10 + 第4秒10 = 38 (4s平均 9.5/s)
	{modelKey: "viduq2", actionType: "generate", resolution: "720p", creditsPerSecond: 10}, // turbo 近似
	{modelKey: "viduq2", actionType: "generate", resolution: "540p", creditsPerSecond: 5},  // turbo 近似
	{modelKey: "viduq2", actionType: "generate", resolution: "1080p", creditsPerSecond: 18}, // turbo 近似
	{modelKey: "viduq2", actionType: "first_tail", resolution: "720p", creditsPerSecond: 10},
	{modelKey: "viduq2", actionType: "first_tail", resolution: "540p", creditsPerSecond: 5},
	{modelKey: "viduq2", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 18},

	// --- Q2 参考生（只能用 viduq2 纯模型名） ---
	{modelKey: "viduq2", actionType: "reference", resolution: "720p", creditsPerSecond: 8},  // 近似
	{modelKey: "viduq2", actionType: "reference", resolution: "540p", creditsPerSecond: 5},
	{modelKey: "viduq2", actionType: "reference", resolution: "1080p", creditsPerSecond: 20},

	// ===== ViduQ1 系列（固定 5 秒，固定价格） =====
	// 文档位置：vidu 产品定价.md 第 87-97 行
	// 所有动作统一 80 积分/次（5秒），折合 16 积分/秒
	{modelKey: "viduq1", actionType: "generate", resolution: "1080p", creditsPerSecond: 16},
	{modelKey: "viduq1", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 16},
	{modelKey: "viduq1", actionType: "reference", resolution: "1080p", creditsPerSecond: 16},

	// ===== Vidu 2.0 系列（固定 4s/8s） =====
	// 文档位置：vidu 产品定价.md 第 99-113 行
	// 图生/首尾帧 720p 4s = 40 积分 → 10 积分/秒
	// 参考 720p 4s = 80 积分 → 20 积分/秒（2倍！）

	// --- 图生视频 / 首尾帧 ---
	{modelKey: "vidu2.0", actionType: "generate", resolution: "360p", creditsPerSecond: 5},  // 20/4s
	{modelKey: "vidu2.0", actionType: "generate", resolution: "720p", creditsPerSecond: 10}, // 40/4s
	{modelKey: "vidu2.0", actionType: "generate", resolution: "1080p", creditsPerSecond: 25}, // 100/4s
	{modelKey: "vidu2.0", actionType: "first_tail", resolution: "360p", creditsPerSecond: 5},
	{modelKey: "vidu2.0", actionType: "first_tail", resolution: "720p", creditsPerSecond: 10},
	{modelKey: "vidu2.0", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 25},

	// --- 参考视频（价格是图生的 2 倍！） ---
	{modelKey: "vidu2.0", actionType: "reference", resolution: "360p", creditsPerSecond: 20}, // 80/4s
	{modelKey: "vidu2.0", actionType: "reference", resolution: "720p", creditsPerSecond: 20}, // 80/4s
	{modelKey: "vidu2.0", actionType: "reference", resolution: "1080p", creditsPerSecond: 25}, // 100/4s（官方1080p参考也是100）

	// ===== Vidu 1.5 系列（旧版，与 Q1 同价） =====
	{modelKey: "vidu1.5", actionType: "generate", resolution: "1080p", creditsPerSecond: 16},
	{modelKey: "vidu1.5", actionType: "first_tail", resolution: "1080p", creditsPerSecond: 16},
	{modelKey: "vidu1.5", actionType: "reference", resolution: "1080p", creditsPerSecond: 16},
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
}

// EstimateBilling 实现 Vidu 动态计费逻辑，与官网定价完全一致。
//
// 计费流程：
//  1. 从请求中提取：模型名称、动作类型、时长（秒）、分辨率
//  2. 查询 viduPricingTable 获取每秒积分消耗
//  3. 计算总积分 = 每秒积分 × 秒数
//  4. 将总积分转换为系统 OtherRatios 倍率（multiplier）
//
// 返回值说明：
//  - map["vidu_billing"] = 费用倍率（总积分 × creditsPerRUnit）
//  - RelayTaskSubmit 会将此倍率乘以基础 Quota 得到最终预扣额度
//  - 最终公式：费用(元) = (ModelRatio/2) × 总积分 × creditsPerRUnit × GroupRatio
//
// 前端配置要求：
//  - 需要在「运营设置 → 分组与模型定价」中为每个 vidu 模型设置 ModelRatio
//  - 推荐将 ModelRatio 统一设为 1，实际费用由本方法动态计算
//  - 设为 1 时：费用(元) = 总积分 × ¥0.03125（与官网完全一致）
func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	// remix 路径的 OtherRatios 已在 ResolveOriginTask 中设置，此处跳过
	if info.Action == constant.TaskActionRemix {
		return nil
	}

	// 获取用户请求数据
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}

	// 提取时长（秒）：优先从 seconds 字符串解析，其次用 duration 整数字段
	seconds := 0
	if req.Seconds != "" {
		if s, err := strconv.Atoi(req.Seconds); err == nil && s > 0 {
			seconds = s
		}
	}
	if seconds <= 0 {
		seconds = req.Duration
	}
	if seconds <= 0 {
		// 根据模型系列设定默认时长
		modelName := info.UpstreamModelName
		if strings.Contains(modelName, "viduq1") || strings.Contains(modelName, "vidu1.5") {
			seconds = 5 // Q1 / 1.5 固定 5 秒
		} else if strings.Contains(modelName, "vidu2.0") {
			seconds = 4 // 2.0 固定 4 秒（默认）
		} else {
			seconds = 4 // Q2/Q3 默认 4 秒
		}
	}

	// 提取分辨率并标准化为 Vidu 格式（540p / 720p / 1080p）
	resolution := normalizeViduResolution(req.Size)

	// 确定动作类型
	actionType := viduActionToString(info.Action)

	// 查询每秒积分消耗
	modelName := info.UpstreamModelName
	creditsPerSec := lookupViduCredits(modelName, actionType, resolution)
	if creditsPerSec <= 0 {
		// 未找到匹配的定价条目，返回 nil 让系统使用默认 ModelRatio
		return nil
	}

	// 计算总积分消耗
	totalCredits := creditsPerSec * seconds

	// 将积分转换为系统 quota 倍率
	// 公式：multiplier = totalCredits × creditsPerRUnit
	// 其中 creditsPerRUnit = 0.0625（当 USD2RMB=1 时，推导见常量注释）
	multiplier := float64(totalCredits) * creditsPerRUnit

	// 构建返回值：包含计费倍率和诊断信息。
	//
	// 计费 key（无前缀）：会被 relay_task.go 乘入 baseQuota，参与实际扣费
	// 诊断 key（_ 前缀）：仅用于日志记录，不参与计费计算（relay_task.go 会跳过）
	//
	// 日志输出示例：
	//   操作 generate, 计算参数：vidu_billing: 2.50,
	//   [vidu] model=viduq3-pro, action=generate, res=720p, duration=4s,
	//   cps=20, credits=80, cost=¥2.50
	result := map[string]float64{
		"vidu_billing": multiplier, // ← 计费倍率（唯一参与扣费的值）
	}

	// 以下 _ 前缀 key 为诊断信息，仅供日志展示，不参与费用计算
	result["_vidu_seconds"] = float64(seconds)                                    // 实际时长(秒)
	result["_vidu_resolution"] = viduResolutionToCode(resolution)                 // 分辨率编码
	result["_vidu_action"] = viduActionToCode(actionType)                        // 动作类型编码
	result["_vidu_cps"] = float64(creditsPerSec)                                // 每秒积分
	result["_vidu_total_credits"] = float64(totalCredits)                       // 总积分
	result["_vidu_cost_rmb"] = float64(totalCredits) * creditPriceRMB          // 官方价格(元)

	return result
}

// AdjustBillingOnSubmit 在任务提交成功后，根据上游返回的实际参数调整计费。
//
// Vidu API 在提交响应中不返回实际的时长/分辨率等参数，
// 因此无需调整，返回 nil 保持预扣额度不变。
// 如果未来 Vidu API 在响应中返回了实际消耗的积分（credits 字段），
// 可以在此处实现精确调整逻辑。
func (a *TaskAdaptor) AdjustBillingOnSubmit(_ *relaycommon.RelayInfo, _ []byte) map[string]float64 {
	return nil
}

// AdjustBillingOnComplete 在任务轮询到达终态时，根据实际结果调整最终计费。
//
// Vidu API 在任务完成响应中包含 credits 字段（实际消耗积分），
// 可用于精确结算。当前实现返回 0 保持预扣额度不变，
// 因为 EstimateBilling 已根据请求参数精确计算了预扣额度。
// 如需基于实际消耗积分进行差额结算，可在此处返回实际 quota 值。
func (a *TaskAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	if err := relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate); err != nil {
		return err
	}
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return service.TaskErrorWrapper(err, "get_task_request_failed", http.StatusBadRequest)
	}
	action := constant.TaskActionTextGenerate
	if meatAction, ok := req.Metadata["action"]; ok {
		action, _ = meatAction.(string)
	} else if req.HasImage() {
		action = constant.TaskActionGenerate
		if info.ChannelType == constant.ChannelTypeVidu {
			// vidu 增加 首尾帧生视频和参考图生视频
			if len(req.Images) == 2 {
				action = constant.TaskActionFirstTailGenerate
			} else if len(req.Images) > 2 {
				action = constant.TaskActionReferenceGenerate
			}
		}
	}
	info.Action = action
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	v, exists := c.Get("task_request")
	if !exists {
		return nil, fmt.Errorf("request not found in context")
	}
	req := v.(relaycommon.TaskSubmitReq)

	body, err := a.convertToRequestPayload(&req, info)
	if err != nil {
		return nil, err
	}

	if info.Action == constant.TaskActionReferenceGenerate {
		// 参考图生视频有模型名限制：
		//   - Q2 系列：只能用 "viduq2" 纯模型名（不能带 pro/turbo 后缀）
		//   - Q3 系列：pro/turbo 需去掉后缀，使用标准版 viduq3
		// 参考：https://platform.vidu.cn/docs/reference-to-video
		model := body.Model
		if strings.Contains(model, "viduq2") {
			body.Model = "viduq2"
		} else if strings.Contains(model, "viduq3-pro") || strings.Contains(model, "viduq3-turbo") {
			body.Model = "viduq3"
		}
	}

	data, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	var path string
	switch info.Action {
	case constant.TaskActionGenerate:
		path = "/img2video"
	case constant.TaskActionFirstTailGenerate:
		path = "/start-end2video"
	case constant.TaskActionReferenceGenerate:
		path = "/reference2video"
	default:
		path = "/text2video"
	}
	return fmt.Sprintf("%s/ent/v2%s", a.baseURL, path), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Token "+info.ApiKey)
	return nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}

	var vResp responsePayload
	err = common.Unmarshal(responseBody, &vResp)
	if err != nil {
		taskErr = service.TaskErrorWrapper(errors.Wrap(err, fmt.Sprintf("%s", responseBody)), "unmarshal_response_failed", http.StatusInternalServerError)
		return
	}

	if vResp.State == "failed" {
		taskErr = service.TaskErrorWrapperLocal(fmt.Errorf("task failed"), "task_failed", http.StatusBadRequest)
		return
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.CreatedAt = time.Now().Unix()
	ov.Model = info.OriginModelName
	c.JSON(http.StatusOK, ov)
	return vResp.TaskId, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	url := fmt.Sprintf("%s/ent/v2/tasks/%s/creations", baseUrl, taskID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Token "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

// GetModelList 返回 Vidu 渠道支持的所有模型名称。
//
// 支持的模型系列：
//   - ViduQ3（最新）：viduq3-pro, viduq3-turbo, viduq3-pro-fast, viduq3-mix, viduq3
//   - ViduQ2：viduq2
//   - ViduQ1：viduq1
//   - Vidu 2.0：vidu2.0
//   - Vidu 1.5：vidu1.5
func (a *TaskAdaptor) GetModelList() []string {
	return []string{
		// Q3 系列（最新，支持 1-16 秒变长）
		"viduq3-pro",
		"viduq3-turbo",
		"viduq3-pro-fast",
		"viduq3-mix",
		"viduq3",
		// Q2 系列（按秒递增定价）
		"viduq2",
		// Q1 系列（固定 5 秒）
		"viduq1",
		// 2.0 系列（固定 4s/8s）
		"vidu2.0",
		// 1.5 旧版
		"vidu1.5",
	}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "vidu"
}

// ============================
// helpers
// ============================

func (a *TaskAdaptor) convertToRequestPayload(req *relaycommon.TaskSubmitReq, info *relaycommon.RelayInfo) (*requestPayload, error) {
	r := requestPayload{
		Model:             taskcommon.DefaultString(info.UpstreamModelName, "viduq1"),
		Images:            req.Images,
		Prompt:            req.Prompt,
		Duration:          taskcommon.DefaultInt(req.Duration, 5),
		Resolution:        taskcommon.DefaultString(req.Size, "1080p"),
		MovementAmplitude: "auto",
		Bgm:               false,
	}
	if err := taskcommon.UnmarshalMetadata(req.Metadata, &r); err != nil {
		return nil, errors.Wrap(err, "unmarshal metadata failed")
	}
	return &r, nil
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	taskInfo := &relaycommon.TaskInfo{}

	var taskResp taskResultResponse
	err := common.Unmarshal(respBody, &taskResp)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response body")
	}

	state := taskResp.State
	switch state {
	case "created", "queueing":
		taskInfo.Status = model.TaskStatusSubmitted
	case "processing":
		taskInfo.Status = model.TaskStatusInProgress
	case "success":
		taskInfo.Status = model.TaskStatusSuccess
		if len(taskResp.Creations) > 0 {
			taskInfo.Url = taskResp.Creations[0].URL
		}
	case "failed":
		taskInfo.Status = model.TaskStatusFailure
		if taskResp.ErrCode != "" {
			taskInfo.Reason = taskResp.ErrCode
		}
	default:
		return nil, fmt.Errorf("unknown task state: %s", state)
	}

	return taskInfo, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	var viduResp taskResultResponse
	if err := common.Unmarshal(originTask.Data, &viduResp); err != nil {
		return nil, errors.Wrap(err, "unmarshal vidu task data failed")
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.CompletedAt = originTask.UpdatedAt

	if len(viduResp.Creations) > 0 && viduResp.Creations[0].URL != "" {
		openAIVideo.SetMetadata("url", viduResp.Creations[0].URL)
	}

	if viduResp.State == "failed" && viduResp.ErrCode != "" {
		openAIVideo.Error = &dto.OpenAIVideoError{
			Message: viduResp.ErrCode,
			Code:    viduResp.ErrCode,
		}
	}

	return common.Marshal(openAIVideo)
}

// ============================
// 计费辅助函数
// ============================

// lookupViduCredits 从定价表中查询指定模型、动作类型和分辨率的每秒积分消耗。
//
// 匹配规则（按优先级）：
//  1. 精确匹配 modelKey（不使用前缀）+ actionType + resolution
//  2. 前缀匹配（处理 viduq3-pro-fast 等变体）+ actionType + resolution
//     注意：优先匹配最长的 modelKey（避免 viduq3-pro-fast 匹配到 viduq3-pro）
//  3. 同模型+动作，回退到 720p 分辨率（最常用分辨率）
//  4. 仅匹配模型前缀，取第一个匹配到的条目
//
// 参数：
//   - model: 上游模型名称（如 "viduq3-pro"、"viduq2.0"）
//   - actionType: 动作类型字符串（"generate"、"first_tail"、"reference"）
//   - resolution: 分辨率（"540p"、"720p"、"1080p"）
//
// 返回值：
//   - 正数：每秒积分消耗
//   - 0：未找到匹配的定价条目
func lookupViduCredits(model, actionType, resolution string) int {
	// 第一轮：精确匹配 modelKey（完整名称匹配）
	for _, entry := range viduPricingTable {
		if entry.modelKey == model &&
			entry.actionType == actionType &&
			entry.resolution == resolution {
			return entry.creditsPerSecond
		}
	}

	// 第二轮：前缀匹配，优先最长前缀（避免 viduq3-pro-fast 匹配到 viduq3-pro）
	bestMatch := ""
	bestCps := 0
	for _, entry := range viduPricingTable {
		if strings.HasPrefix(model, entry.modelKey) &&
			entry.actionType == actionType &&
			entry.resolution == resolution {
			// 选择最长的匹配 modelKey（最精确的匹配）
			if len(entry.modelKey) > len(bestMatch) {
				bestMatch = entry.modelKey
				bestCps = entry.creditsPerSecond
			}
		}
	}
	if bestCps > 0 {
		return bestCps
	}

	// 第三轮：同模型+动作，回退到 720p 分辨率
	for _, entry := range viduPricingTable {
		if (entry.modelKey == model || strings.HasPrefix(model, entry.modelKey)) &&
			entry.actionType == actionType &&
			entry.resolution == "720p" {
			return entry.creditsPerSecond
		}
	}

	// 第四轮：仅匹配模型前缀，取第一个匹配到的条目
	for _, entry := range viduPricingTable {
		if strings.HasPrefix(model, entry.modelKey) {
			return entry.creditsPerSecond
		}
	}

	// 未找到任何匹配
	return 0
}

// normalizeViduResolution 将用户传入的分辨率标准化为 Vidu API 支持的格式。
//
// 支持的输入格式：
//   - Vidu 原生格式："540p"、"720p"、"1080p"
//   - OpenAI 格式："720x1280"、"1024x1792"、"1792x1024"、"1280x720"
//   - 数字格式：纯数字字符串
//
// 映射规则（短边判定）：
//   - 短边 ≤ 600 → "540p"
//   - 短边 ≤ 900 → "720p"
//   - 其他       → "1080p"
func normalizeViduResolution(size string) string {
	if size == "" {
		return "720p" // 默认使用 720p（性价比最高）
	}

	// 已经是 Vidu 标准格式，直接返回
	switch size {
	case "360p", "540p", "720p", "1080p":
		return size
	}

	// 尝试解析 "WxH" 格式
	var width, height int
	n, _ := fmt.Sscanf(size, "%dx%d", &width, &height)
	if n == 2 && width > 0 && height > 0 {
		shortSide := width
		if height < shortSide {
			shortSide = height
		}
		if shortSide <= 600 {
			return "540p"
		} else if shortSide <= 900 {
			return "720p"
		}
		return "1080p"
	}

	// 无法识别的格式，默认返回 720p
	return "720p"
}

// viduActionToString 将系统内部的动作常量转换为计费表使用的动作类型字符串。
func viduActionToString(action string) string {
	switch action {
	case constant.TaskActionGenerate:
		return "generate" // 图生视频 / 文生视频
	case constant.TaskActionFirstTailGenerate:
		return "first_tail" // 首尾帧生视频
	case constant.TaskActionReferenceGenerate:
		return "reference" // 参考图生视频
	default:
		return "generate" // 默认按标准生成处理
	}
}

// viduResolutionToCode 将分辨率字符串转换为数值编码（用于日志诊断）。
//
// 编码规则：
//   1 = 360p, 2 = 540p, 3 = 720p, 4 = 1080p
func viduResolutionToCode(resolution string) float64 {
	switch resolution {
	case "360p":
		return 1
	case "540p":
		return 2
	case "720p":
		return 3
	case "1080p":
		return 4
	default:
		return 3 // 默认 720p
	}
}

// viduActionToCode 将动作类型字符串转换为数值编码（用于日志诊断）。
//
// 编码规则：
//   1 = generate(图生/文生), 2 = first_tail(首尾帧), 3 = reference(参考生)
func viduActionToCode(actionType string) float64 {
	switch actionType {
	case "generate":
		return 1
	case "first_tail":
		return 2
	case "reference":
		return 3
	default:
		return 1 // 默认标准生成
	}
}
