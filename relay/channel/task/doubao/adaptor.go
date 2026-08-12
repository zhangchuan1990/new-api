package doubao

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/samber/lo"
)

// ============================
// Request / Response structures
// ============================

type ContentItem struct {
	Type     string    `json:"type,omitempty"`
	Text     string    `json:"text,omitempty"`
	ImageURL *MediaURL `json:"image_url,omitempty"`
	VideoURL *MediaURL `json:"video_url,omitempty"`
	AudioURL *MediaURL `json:"audio_url,omitempty"`
	Role     string    `json:"role,omitempty"`
}

type MediaURL struct {
	URL string `json:"url,omitempty"`
}

type requestPayload struct {
	Model                 string         `json:"model"`
	Content               []ContentItem  `json:"content,omitempty"`
	CallbackURL           string         `json:"callback_url,omitempty"`
	ReturnLastFrame       *dto.BoolValue `json:"return_last_frame,omitempty"`
	ServiceTier           string         `json:"service_tier,omitempty"`
	ExecutionExpiresAfter *dto.IntValue  `json:"execution_expires_after,omitempty"`
	GenerateAudio         *dto.BoolValue `json:"generate_audio,omitempty"`
	Draft                 *dto.BoolValue `json:"draft,omitempty"`
	Tools                 []struct {
		Type string `json:"type,omitempty"`
	} `json:"tools,omitempty"`
	SafetyIdentifier string         `json:"safety_identifier,omitempty"`
	Priority         *dto.IntValue  `json:"priority,omitempty"`
	Resolution       string         `json:"resolution,omitempty"`
	Ratio            string         `json:"ratio,omitempty"`
	Duration         *dto.IntValue  `json:"duration,omitempty"`
	Frames           *dto.IntValue  `json:"frames,omitempty"`
	Seed             *dto.IntValue  `json:"seed,omitempty"`
	CameraFixed      *dto.BoolValue `json:"camera_fixed,omitempty"`
	Watermark        *dto.BoolValue `json:"watermark,omitempty"`
}

type responsePayload struct {
	ID string `json:"id"` // task_id
}

// ResponseTask 火山方舟视频任务查询响应结构。
// 导出供 doubao_newapi 适配器复用（解析 NewAPI 封装内层的原始响应）。
type ResponseTask struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Seed            int    `json:"seed"`
	Resolution      string `json:"resolution"`
	Duration        int    `json:"duration"`
	Ratio           string `json:"ratio"`
	FramesPerSecond int    `json:"framespersecond"`
	ServiceTier     string `json:"service_tier"`
	Tools           []struct {
		Type string `json:"type"`
	} `json:"tools"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		ToolUsage        struct {
			WebSearch int `json:"web_search"`
		} `json:"tool_usage"`
	} `json:"usage"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// ============================
// Adaptor implementation
// ============================

type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	apiKey      string
	baseURL     string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

// ValidateRequestAndSetAction parses body, validates fields and sets default action.
func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) (taskErr *taskdto.TaskError) {
	// Accept only POST /v1/video/generations as "generate" action.
	return relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate)
}

// BuildRequestURL constructs the upstream URL.
func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/api/v3/contents/generations/tasks", a.baseURL), nil
}

// BuildRequestHeader sets required headers.
func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return nil
}

// EstimateBilling 检测请求 metadata 中的视频输入和分辨率，返回对应的 OtherRatio。
//
// 计费逻辑：管理员设置 ModelRatio 为 720P 不含视频的较高费率，
// 系统根据视频输入和分辨率自动乘以折扣：
//   - 720P 含视频: videoInputRatio（含视频单价 / 720P不含视频单价）
//   - 1080P 不含视频: resolutionRatio1080P（1080P不含视频单价 / 720P不含视频单价）
//   - 1080P 含视频: videoInputRatio1080P（1080P含视频单价 / 720P不含视频单价，已包含分辨率折扣）
//
// 返回值包含两类 key：
//   - 计费 key（无前缀）：会被 relay_task.go 乘入 baseQuota，参与实际扣费
//     （video_input / resolution）
//   - 诊断 key（_ 前缀）：仅用于日志记录，不参与计费计算
//     （_doubao_resolution, _doubao_has_video, _doubao_ratio_type 等）
//
// 日志输出示例：
//   操作 generate, 计算参数：video_input: 0.61,
//   [doubao] 分辨率: 720p, 视频输入: 是, 倍率类型: 视频折扣(720p), 官网基准: ¥46/1M
func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}

	// 构建用于计费的 metadata：优先使用客户端显式传入的 metadata，
	// 对于缺失的 resolution，从原始请求体顶层补充（火山方舟 API 标准位置）。
	// TaskSubmitReq 不含 Resolution 字段，客户端按火山方舟标准将 resolution 放在顶层时，
	// 需要在此补充到 metadata 中，否则计费逻辑无法识别 1080p 等分辨率。
	billingMetadata := make(map[string]interface{})
	for k, v := range req.Metadata {
		billingMetadata[k] = v
	}
	if _, ok := billingMetadata["resolution"]; !ok {
		if storage, err := common.GetBodyStorage(c); err == nil {
			if bodyBytes, err := storage.Bytes(); err == nil {
				var topFields struct {
					Resolution string `json:"resolution"`
				}
				if err := common.Unmarshal(bodyBytes, &topFields); err == nil && topFields.Resolution != "" {
					billingMetadata["resolution"] = topFields.Resolution
				}
			}
		}
	}

	return EstimateBillingFromMetadata(info.OriginModelName, billingMetadata, req.Seconds, req.Duration)
}

// EstimateBillingFromMetadata 根据模型名和请求 metadata 计算豆包视频的计费倍率。
// 此函数为包级函数，供 doubao 适配器和 doubao_newapi 适配器共用计费逻辑。
//
// 参数：
//   - modelName: 模型名称（如 doubao-seedance-2-0-260128）
//   - metadata: 请求中的 metadata 字段（包含 resolution、content 等信息）
//   - secondsStr: 时长字符串（如 "4"）
//   - duration: 时长整数值（当 secondsStr 为空时的回退值）
//
// 返回值：
//   - map[string]float64: 计费倍率映射，key 为 video_input/resolution 等计费维度
//     以及 _doubao_ 前缀的诊断信息（仅用于日志，不参与计费）
func EstimateBillingFromMetadata(
	modelName string,
	metadata map[string]interface{},
	secondsStr string,
	duration int,
) map[string]float64 {
	hasVideo := hasVideoInMetadata(metadata)
	resolution := getResolutionFromMetadata(metadata)
	is1080P := Is1080P(resolution)

	ratios := make(map[string]float64)

	// 提取时长（秒），用于日志展示
	seconds := 0
	if secondsStr != "" {
		if s, err := strconv.Atoi(secondsStr); err == nil && s > 0 {
			seconds = s
		}
	}
	if seconds <= 0 {
		seconds = duration
	}

	if is1080P {
		// 1080P 分辨率
		if hasVideo {
			// 1080P 含视频输入：使用 1080P 视频输入折扣（已包含分辨率折扣）
			if ratio, ok := GetVideoInputRatio1080P(modelName); ok {
				ratios["video_input"] = ratio
				appendDoubaoDiag(ratios, modelName, "1080p", true, seconds, ratio, 3)
			}
		} else {
			// 1080P 不含视频输入：使用分辨率折扣
			if ratio, ok := GetResolutionRatio1080P(modelName); ok {
				ratios["resolution"] = ratio
				appendDoubaoDiag(ratios, modelName, "1080p", false, seconds, ratio, 2)
			}
		}
	} else {
		// 720P/480P 分辨率（默认）
		if hasVideo {
			if ratio, ok := GetVideoInputRatio(modelName); ok {
				ratios["video_input"] = ratio
				appendDoubaoDiag(ratios, modelName, "720p", true, seconds, ratio, 1)
			}
		} else {
			// 720P 不含视频：使用基准 ModelRatio，无额外倍率，但仍输出诊断信息
			appendDoubaoDiag(ratios, modelName, "720p", false, seconds, 1.0, 0)
		}
	}

	if len(ratios) == 0 {
		return nil
	}
	return ratios
}

// getResolutionFromMetadata 从 metadata 中提取分辨率信息。
// 优先读取 metadata.resolution 字段。
func getResolutionFromMetadata(metadata map[string]interface{}) string {
	if metadata == nil {
		return ""
	}
	if resolution, ok := metadata["resolution"].(string); ok && resolution != "" {
		return resolution
	}
	return ""
}

// hasVideoInMetadata 直接检查 metadata 的 content 数组是否包含 video_url 条目，
// 避免构建完整的上游 requestPayload。
func hasVideoInMetadata(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	contentRaw, ok := metadata["content"]
	if !ok {
		return false
	}
	contentSlice, ok := contentRaw.([]interface{})
	if !ok {
		return false
	}
	for _, item := range contentSlice {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if itemMap["type"] == "video_url" {
			return true
		}
		if _, has := itemMap["video_url"]; has {
			return true
		}
	}
	return false
}

// BuildRequestBody converts request into Doubao specific format.
func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, err
	}

	// 补充从原始请求体顶层提取火山方舟标准字段到 metadata。
	// TaskSubmitReq 不含 Resolution/GenerateAudio 等字段，客户端按火山方舟 API 标准
	// 将这些字段放在顶层时，需要在此补充到 metadata 中，否则 convertToRequestPayload
	// 的 UnmarshalMetadata 读不到，上游请求会丢失这些字段。
	if req.Metadata == nil {
		req.Metadata = make(map[string]interface{})
	}
	if storage, err := common.GetBodyStorage(c); err == nil {
		if bodyBytes, err := storage.Bytes(); err == nil {
			var topFields struct {
				Resolution    string `json:"resolution"`
				GenerateAudio *bool  `json:"generate_audio"`
			}
			if err := common.Unmarshal(bodyBytes, &topFields); err == nil {
				if _, ok := req.Metadata["resolution"]; !ok && topFields.Resolution != "" {
					req.Metadata["resolution"] = topFields.Resolution
				}
				// generate_audio 使用指针区分"未发送"（nil）和"显式发送 false"
				if _, ok := req.Metadata["generate_audio"]; !ok && topFields.GenerateAudio != nil {
					req.Metadata["generate_audio"] = *topFields.GenerateAudio
				}
			}
		}
	}

	body, err := a.convertToRequestPayload(&req)
	if err != nil {
		return nil, errors.Wrap(err, "convert request payload failed")
	}
	if info.IsModelMapped {
		body.Model = info.UpstreamModelName
	} else {
		info.UpstreamModelName = body.Model
	}
	data, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// DoRequest delegates to common helper.
func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// DoResponse handles upstream response, returns taskID etc.
func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *taskdto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}
	_ = resp.Body.Close()

	// Parse Doubao response
	var dResp responsePayload
	if err := common.Unmarshal(responseBody, &dResp); err != nil {
		taskErr = service.TaskErrorWrapper(errors.Wrapf(err, "body: %s", responseBody), "unmarshal_response_body_failed", http.StatusInternalServerError)
		return
	}

	if dResp.ID == "" {
		taskErr = service.TaskErrorWrapper(fmt.Errorf("task_id is empty"), "invalid_response", http.StatusInternalServerError)
		return
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.CreatedAt = time.Now().Unix()
	ov.Model = info.OriginModelName

	c.JSON(http.StatusOK, ov)
	return dResp.ID, responseBody, nil
}

// FetchTask fetch task status
func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	uri := fmt.Sprintf("%s/api/v3/contents/generations/tasks/%s", baseUrl, taskID)

	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) convertToRequestPayload(req *relaycommon.TaskSubmitReq) (*requestPayload, error) {
	r := requestPayload{
		Model:   req.Model,
		Content: []ContentItem{},
	}

	// Add images if present
	if req.HasImage() {
		for _, imgURL := range req.Images {
			r.Content = append(r.Content, ContentItem{
				Type: "image_url",
				ImageURL: &MediaURL{
					URL: imgURL,
				},
			})
		}
	}

	metadata := req.Metadata
	if err := taskcommon.UnmarshalMetadata(metadata, &r); err != nil {
		return nil, errors.Wrap(err, "unmarshal metadata failed")
	}

	// 时长设置：优先使用 Seconds（字符串），回退到 Duration（int）。
	// 客户端按火山方舟 API 标准发送 duration 数字字段时，会解析到 req.Duration，
	// 仅处理 req.Seconds 会导致 duration 丢失，上游按默认时长生成。
	if sec, _ := strconv.Atoi(req.Seconds); sec > 0 {
		r.Duration = lo.ToPtr(dto.IntValue(sec))
	} else if req.Duration > 0 {
		r.Duration = lo.ToPtr(dto.IntValue(req.Duration))
	}

	r.Content = lo.Reject(r.Content, func(c ContentItem, _ int) bool { return c.Type == "text" })
	r.Content = append(r.Content, ContentItem{
		Type: "text",
		Text: req.Prompt,
	})

	return &r, nil
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	resTask := ResponseTask{}
	if err := common.Unmarshal(respBody, &resTask); err != nil {
		return nil, errors.Wrap(err, "unmarshal task result failed")
	}

	taskResult := relaycommon.TaskInfo{
		Code: 0,
	}

	// Map Doubao status to internal status
	switch resTask.Status {
	case "pending", "queued":
		taskResult.Status = model.TaskStatusQueued
		taskResult.Progress = "10%"
	case "processing", "running":
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "50%"
	case "succeeded":
		taskResult.Status = model.TaskStatusSuccess
		taskResult.Progress = "100%"
		taskResult.Url = resTask.Content.VideoURL
		// 解析 usage 信息用于按倍率计费
		taskResult.CompletionTokens = resTask.Usage.CompletionTokens
		taskResult.TotalTokens = resTask.Usage.TotalTokens
	case "failed":
		taskResult.Status = model.TaskStatusFailure
		taskResult.Progress = "100%"
		taskResult.Reason = resTask.Error.Message
	default:
		// Unknown status, treat as processing
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "30%"
	}

	return &taskResult, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	var dResp ResponseTask
	if err := common.Unmarshal(originTask.Data, &dResp); err != nil {
		return nil, errors.Wrap(err, "unmarshal doubao task data failed")
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.TaskID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	openAIVideo.SetMetadata("url", dResp.Content.VideoURL)
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.CompletedAt = originTask.UpdatedAt
	openAIVideo.Model = originTask.Properties.OriginModelName

	if dResp.Status == "failed" {
		openAIVideo.Error = &dto.OpenAIVideoError{
			Message: dResp.Error.Message,
			Code:    dResp.Error.Code,
		}
	}

	return common.Marshal(openAIVideo)
}

// ---------------------------------------------------------------------------
// Doubao 诊断信息辅助函数
// ---------------------------------------------------------------------------

// appendDoubaoDiag 向 ratios map 追加 _doubao_ 前缀的诊断信息（仅用于日志，不参与计费）。
//
// 参数说明：
//   - modelName: 模型名称（如 doubao-seedance-2-0-260128）
//   - resolution: 分辨率字符串（"720p" 或 "1080p"）
//   - hasVideo: 是否包含视频输入
//   - seconds: 视频时长（秒）
//   - appliedRatio: 实际应用的倍率值
//   - ratioTypeCode: 倍率类型编码（0=基准无折扣, 1=视频折扣720p, 2=分辨率加价1080p, 3=视频折扣1080p）
func appendDoubaoDiag(
	ratios map[string]float64,
	modelName string,
	resolution string,
	hasVideo bool,
	seconds int,
	appliedRatio float64,
	ratioTypeCode int,
) {
	// 分辨率编码：2=720p, 4=1080p
	resCode := float64(2)
	if resolution == "1080p" {
		resCode = 4
	}

	// 是否有视频输入：1=是, 0=否
	videoFlag := float64(0)
	if hasVideo {
		videoFlag = 1
	}

	// 查询官网基准价格（720P 不含视频单价，单位：元/百万token）
	basePrice := getDoubaoBasePrice(modelName)

	ratios["_doubao_resolution"] = resCode           // 分辨率编码
	ratios["_doubao_has_video"] = videoFlag            // 是否有视频输入
	ratios["_doubao_seconds"] = float64(seconds)        // 时长(秒)
	ratios["_doubao_ratio_type"] = float64(ratioTypeCode) // 倍率类型编码
	ratios["_doubao_ratio_value"] = appliedRatio        // 实际倍率值
	ratios["_doubao_base_price"] = basePrice            // 官网基准价(元/1M tokens)
}

// getDoubaoBasePrice 返回指定模型在 720P 不含视频模式下的官网基准价格（元/百万token）。
// 数据来源：火山方舟官方定价页面。
func getDoubaoBasePrice(modelName string) float64 {
	switch modelName {
	case "doubao-seedance-2-0-260128":
		return 46.0 // 官网：720P 不含视频 = ¥46/1M tokens
	case "doubao-seedance-2-0-fast-260128":
		return 37.0 // 官网：720P 不含视频 = ¥37/1M tokens
	default:
		return 0 // 未知模型
	}
}
