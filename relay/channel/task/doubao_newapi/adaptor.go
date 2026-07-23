package doubao_newapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/doubao"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// ============================
// 响应结构体定义
// ============================

// submitResponse 提交任务响应（OpenAI Sora 格式）。
// 第三方 NewAPI 在提交任务时统一返回此格式，无论路径是 /v1/videos 还是 /v1/video/generations。
type submitResponse struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id,omitempty"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	CreatedAt int64  `json:"created_at"`
}

// newapiWrap NewAPI 封装格式（查询任务时返回）。
// 第三方 NewAPI 在 GET /v1/video/generations/{id} 路径返回此格式，
// 包含两层 data 嵌套：外层为 NewAPI 任务表字段，内层为上游火山方舟原始响应。
type newapiWrap struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    newapiWrapData `json:"data"`
}

// newapiWrapData NewAPI 封装外层 data 字段。
type newapiWrapData struct {
	ID        int             `json:"id"`
	TaskID    string          `json:"task_id"`
	Status    string          `json:"status"`     // SUCCESS / IN_PROGRESS / FAILURE
	Progress  string          `json:"progress"`   // "50%" / "100%"
	ResultURL string          `json:"result_url"` // NewAPI 封装层的 video URL（可能为空）
	Data      json.RawMessage `json:"data"`       // 火山方舟原始响应（内层，可能为空）
}

// ============================
// 适配器实现
// ============================

// TaskAdaptor 豆包视频 NewAPI 中转兼容适配器。
// 用于通过第三方 NewAPI 中转调用豆包视频模型：
//   - 协议层：走 NewAPI 内部路由（/v1/video/generations）
//   - 计费层：复用 doubao 包的 EstimateBillingFromMetadata 计费逻辑
type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	apiKey      string
	baseURL     string
}

// Init 初始化适配器，从 RelayInfo 中读取通道配置。
func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

// ValidateRequestAndSetAction 校验请求并设置默认 action。
// 支持两种请求格式：
//   - OpenAI 风格：prompt 字符串格式（兼容 sora 等）
//   - 火山方舟标准：content 数组格式（含 type=text 的文本条目）
//
// 对于 content 数组格式，需要从 content 中提取文本作为 prompt，
// 否则 ValidateBasicTaskRequest 会因 prompt 为空而拒绝请求。
func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	// 先尝试标准验证（支持 prompt 字符串格式）
	taskErr := relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate)
	if taskErr == nil {
		return nil
	}

	// 标准验证失败，检查是否为 content 数组格式
	// 读取原始请求体，尝试从 content 数组中提取文本作为 prompt
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return taskErr
	}
	bodyBytes, err := storage.Bytes()
	if err != nil {
		return taskErr
	}

	var bodyFields struct {
		Model   string                   `json:"model"`
		Content []map[string]interface{} `json:"content"`
	}
	if err := common.Unmarshal(bodyBytes, &bodyFields); err != nil {
		return taskErr
	}

	// 从 content 数组中提取文本内容作为 prompt
	promptText := ""
	for _, item := range bodyFields.Content {
		if item["type"] == "text" {
			if text, ok := item["text"].(string); ok && text != "" {
				promptText = text
				break
			}
		}
	}

	// content 数组中没有文本内容，返回原始错误
	if promptText == "" {
		return taskErr
	}

	// 重新解析请求，注入提取的 prompt 后存储到 context
	var req relaycommon.TaskSubmitReq
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return service.TaskErrorWrapper(err, "invalid_request", http.StatusBadRequest)
	}
	req.Prompt = promptText

	// 校验 model 字段
	if req.Model == "" {
		req.Model = bodyFields.Model
	}

	// 存储 task_request 到 context（与 ValidateBasicTaskRequest 内部行为一致）
	info.Action = constant.TaskActionGenerate
	c.Set("task_request", req)
	return nil
}

// BuildRequestURL 构造提交任务的上游 URL。
// 第三方 NewAPI 的提交路径为 /v1/video/generations。
func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/v1/video/generations", a.baseURL), nil
}

// BuildRequestHeader 设置请求头。
func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return nil
}

// EstimateBilling 计费倍率计算，复用 doubao 包的计费逻辑。
//
// 对于 doubao_newapi（透传模式），客户端按火山方舟 API 标准将 resolution 和 content
// 放在请求体顶层（而非 metadata 中），但 EstimateBillingFromMetadata 只从 metadata 读取。
// 因此本方法先从原始请求体顶层提取这些字段，补充到 metadata 中供计费使用，
// 这样客户端只需按火山方舟标准格式发送请求即可正确计费。
func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}

	// 构建用于计费的 metadata：优先使用客户端显式传入的 metadata，
	// 对于缺失的 resolution/content，从原始请求体顶层补充。
	billingMetadata := make(map[string]interface{})
	for k, v := range req.Metadata {
		billingMetadata[k] = v
	}

	// 从原始请求体顶层提取 resolution 和 content（火山方舟 API 标准位置）。
	// doubao_newapi 采用透传策略，客户端按火山方舟标准将字段放在顶层，
	// 需要在此补充到 metadata 中，否则计费逻辑无法识别。
	if storage, err := common.GetBodyStorage(c); err == nil {
		if bodyBytes, err := storage.Bytes(); err == nil {
			var topFields struct {
				Resolution string                   `json:"resolution"`
				Content    []map[string]interface{} `json:"content"`
			}
			if err := common.Unmarshal(bodyBytes, &topFields); err == nil {
				if _, ok := billingMetadata["resolution"]; !ok && topFields.Resolution != "" {
					billingMetadata["resolution"] = topFields.Resolution
				}
				if _, ok := billingMetadata["content"]; !ok && len(topFields.Content) > 0 {
					// 转为 []interface{} 以匹配 hasVideoInMetadata 的类型断言
					contentSlice := make([]interface{}, len(topFields.Content))
					for i, item := range topFields.Content {
						contentSlice[i] = item
					}
					billingMetadata["content"] = contentSlice
				}
			}
		}
	}

	return doubao.EstimateBillingFromMetadata(info.OriginModelName, billingMetadata, req.Seconds, req.Duration)
}

// BuildRequestBody 构造请求体。
// 采用透传策略：保留客户端原始 body，仅注入上游模型名。
// 对于 prompt 字符串格式的请求，自动转换为火山方舟标准的 content 数组格式，
// 否则第三方 NewAPI 不识别 prompt 字段，会忽略 resolution 等参数导致按默认 720P 生成。
func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, errors.Wrap(err, "get_request_body_failed")
	}
	cachedBody, err := storage.Bytes()
	if err != nil {
		return nil, errors.Wrap(err, "read_body_bytes_failed")
	}

	var bodyMap map[string]interface{}
	if err := common.Unmarshal(cachedBody, &bodyMap); err != nil {
		// JSON 解析失败，直接透传原始 body
		return bytes.NewReader(cachedBody), nil
	}
	bodyMap["model"] = info.UpstreamModelName

	// 如果客户端发送的是 prompt 字符串格式，转换为 content 数组格式（火山方舟 API 标准）。
	// 第三方 NewAPI 只识别 content 数组格式，prompt 字符串格式会导致分辨率等参数被忽略。
	if prompt, ok := bodyMap["prompt"].(string); ok && prompt != "" {
		if _, hasContent := bodyMap["content"]; !hasContent {
			bodyMap["content"] = []map[string]interface{}{
				{"type": "text", "text": prompt},
			}
		}
		delete(bodyMap, "prompt")
	}

	data, err := common.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// DoRequest 发送 HTTP 请求。
func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// DoResponse 解析提交任务的上游响应（OpenAI Sora 格式）。
// 返回上游真实 task_id 用于后续轮询，同时向客户端返回 OpenAI Video 格式响应。
func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}
	_ = resp.Body.Close()

	var sResp submitResponse
	if err := common.Unmarshal(responseBody, &sResp); err != nil {
		taskErr = service.TaskErrorWrapper(errors.Wrapf(err, "body: %s", responseBody), "unmarshal_response_body_failed", http.StatusInternalServerError)
		return
	}

	// 提取上游真实 task_id（优先取 id，回退到 task_id）
	upstreamID := sResp.ID
	if upstreamID == "" {
		upstreamID = sResp.TaskID
	}
	if upstreamID == "" {
		taskErr = service.TaskErrorWrapper(fmt.Errorf("task_id is empty"), "invalid_response", http.StatusInternalServerError)
		return
	}

	// 向客户端返回 OpenAI Video 格式响应，使用公开 task_id
	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.CreatedAt = time.Now().Unix()
	ov.Model = info.OriginModelName
	ov.Status = sResp.Status
	ov.SetProgressStr(fmt.Sprintf("%d%%", sResp.Progress))
	c.JSON(http.StatusOK, ov)

	return upstreamID, responseBody, nil
}

// FetchTask 查询任务状态。
// 第三方 NewAPI 的查询路径为 GET /v1/video/generations/{task_id}，
// 返回 NewAPI 封装格式（含两层 data 嵌套）。
func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	uri := fmt.Sprintf("%s/v1/video/generations/%s", baseUrl, taskID)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

// ParseTaskResult 解析查询任务的上游响应（NewAPI 封装格式）。
//
// 响应结构有两层 data 嵌套：
//   - 外层 data：NewAPI 任务表字段（status=SUCCESS/IN_PROGRESS/FAILURE）
//   - 内层 data.data：火山方舟原始响应（status=succeeded/running/failed，含 usage）
//
// 优先解析内层原始响应（与 doubao 适配器逻辑一致），回退到外层状态判断。
func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	var wrap newapiWrap
	if err := common.Unmarshal(respBody, &wrap); err != nil {
		return nil, errors.Wrap(err, "unmarshal newapi wrap failed")
	}

	taskResult := relaycommon.TaskInfo{Code: 0}

	// 优先解析内层火山方舟原始响应（data.data）
	if len(wrap.Data.Data) > 0 {
		var inner doubao.ResponseTask
		if err := common.Unmarshal(wrap.Data.Data, &inner); err == nil {
			switch inner.Status {
			case "pending", "queued":
				taskResult.Status = model.TaskStatusQueued
				taskResult.Progress = "10%"
			case "processing", "running":
				taskResult.Status = model.TaskStatusInProgress
				taskResult.Progress = "50%"
			case "succeeded":
				taskResult.Status = model.TaskStatusSuccess
				taskResult.Progress = "100%"
				taskResult.Url = inner.Content.VideoURL
				// 解析 usage 信息用于按倍率计费（token 重算）
				taskResult.CompletionTokens = inner.Usage.CompletionTokens
				taskResult.TotalTokens = inner.Usage.TotalTokens
			case "failed":
				taskResult.Status = model.TaskStatusFailure
				taskResult.Progress = "100%"
				taskResult.Reason = inner.Error.Message
			default:
				taskResult.Status = model.TaskStatusInProgress
				taskResult.Progress = "30%"
			}
			return &taskResult, nil
		}
	}

	// 回退：用外层 NewAPI 状态判断（内层 data 不存在或解析失败时）
	switch wrap.Data.Status {
	case "SUCCESS":
		taskResult.Status = model.TaskStatusSuccess
		taskResult.Progress = "100%"
		taskResult.Url = wrap.Data.ResultURL
	case "IN_PROGRESS":
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "50%"
	case "FAILURE":
		taskResult.Status = model.TaskStatusFailure
		taskResult.Progress = "100%"
		taskResult.Reason = wrap.Message
	default:
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "30%"
	}
	return &taskResult, nil
}

// GetModelList 返回支持的模型列表，复用 doubao 包的列表。
func (a *TaskAdaptor) GetModelList() []string {
	return doubao.ModelList
}

// AdjustBillingOnComplete 任务完成时的计费调整。
//
// 重要：由于第三方 NewAPI 查询响应的 code 字段为 "success"，
// task_polling.go 会命中 NewAPI 格式分支（第 382 行），绕过 ParseTaskResult，
// 导致 taskResult.TotalTokens 始终为 0，token 重算分支无法触发。
//
// 此方法从 task.Data（NewAPI 封装格式，由轮询阶段存储）中提取内层
// data.data.usage.total_tokens，设置到 taskResult.TotalTokens/CompletionTokens，
// 然后返回 0，让 settleTaskBillingOnComplete 走 token 重算分支
// （RecalculateTaskQuotaByTokens）按实际 token 用量精确结算。
//
// 参数：
//   - task: 任务记录，task.Data 存储了最新的轮询响应体（NewAPI 封装格式）
//   - taskResult: 轮询解析结果（已被 NewAPI 格式分支填充，但缺少 usage 信息）
//
// 返回值：
//   - 0：不直接返回额度，让 token 重算分支处理（需要 taskResult.TotalTokens > 0）
func (a *TaskAdaptor) AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int {
	// task.Data 存储的是 NewAPI 封装格式的完整响应体
	var wrap newapiWrap
	if err := common.Unmarshal(task.Data, &wrap); err != nil {
		return 0
	}

	// 内层 data.data 为空，无法提取 usage
	if len(wrap.Data.Data) == 0 {
		return 0
	}

	// 解析内层火山方舟原始响应
	var inner doubao.ResponseTask
	if err := common.Unmarshal(wrap.Data.Data, &inner); err != nil {
		return 0
	}

	// 提取 usage 信息，设置到 taskResult 供 token 重算使用
	if inner.Usage.TotalTokens > 0 {
		taskResult.TotalTokens = inner.Usage.TotalTokens
		taskResult.CompletionTokens = inner.Usage.CompletionTokens
	}

	// 返回 0，让 settleTaskBillingOnComplete 走 token 重算分支
	return 0
}

// GetChannelName 返回通道名称。
func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

// ConvertToOpenAIVideo 将存储的任务数据转换为 OpenAI Video 格式返回给客户端。
// 需要解析 NewAPI 封装格式，从内层 data.data.content.video_url 或外层 result_url 取视频 URL。
func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	var wrap newapiWrap
	if err := common.Unmarshal(originTask.Data, &wrap); err != nil {
		return nil, errors.Wrap(err, "unmarshal newapi wrap failed")
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.TaskID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.CompletedAt = originTask.UpdatedAt
	openAIVideo.Model = originTask.Properties.OriginModelName

	// 优先从内层 data.data.content.video_url 取视频 URL
	videoURL := ""
	if len(wrap.Data.Data) > 0 {
		var inner doubao.ResponseTask
		if err := common.Unmarshal(wrap.Data.Data, &inner); err == nil {
			videoURL = inner.Content.VideoURL
			if inner.Status == "failed" {
				openAIVideo.Error = &dto.OpenAIVideoError{
					Message: inner.Error.Message,
					Code:    inner.Error.Code,
				}
			}
		}
	}

	// 回退到外层 result_url
	if videoURL == "" {
		videoURL = wrap.Data.ResultURL
	}

	if videoURL != "" {
		openAIVideo.SetMetadata("url", videoURL)
	}

	return common.Marshal(openAIVideo)
}

// ConvertToDoubaoNative 返回火山方舟原生格式响应。
// 供下游 DoubaoVideo 类型渠道对接使用：下游 ParseTaskResult 期望火山方舟原生格式
// （顶层 status=succeeded/running/failed、content.video_url、usage 字段），
// 而中转存储的是 NewAPI 封装格式（双层 data 嵌套），需要提取内层 data.data 原始响应。
//
// 处理逻辑：
//   - 优先返回内层 data.data（火山方舟原始响应，包含 usage 等完整字段）
//   - 回退：根据外层 NewAPI 状态构造简化响应（内层 data 不存在或解析失败时）
//
// 参数：
//   - originTask: 任务记录，task.Data 存储了 NewAPI 封装格式的完整响应体
//
// 返回值：
//   - 火山方舟原生格式的 JSON 字节流
func (a *TaskAdaptor) ConvertToDoubaoNative(originTask *model.Task) ([]byte, error) {
	var wrap newapiWrap
	if err := common.Unmarshal(originTask.Data, &wrap); err != nil {
		return nil, errors.Wrap(err, "unmarshal newapi wrap failed")
	}

	// 优先返回内层 data.data（火山方舟原始响应）
	if len(wrap.Data.Data) > 0 {
		return wrap.Data.Data, nil
	}

	// 回退：根据外层 NewAPI 状态构造简化响应
	status := "running"
	switch wrap.Data.Status {
	case "SUCCESS":
		status = "succeeded"
	case "FAILURE":
		status = "failed"
	case "IN_PROGRESS":
		status = "running"
	}

	fallback := map[string]interface{}{
		"id":     originTask.TaskID,
		"status": status,
	}
	if wrap.Data.ResultURL != "" {
		fallback["content"] = map[string]string{"video_url": wrap.Data.ResultURL}
	}
	if wrap.Message != "" && status == "failed" {
		fallback["error"] = map[string]string{"message": wrap.Message}
	}

	return common.Marshal(fallback)
}
