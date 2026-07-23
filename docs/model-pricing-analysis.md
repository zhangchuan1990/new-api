# 模型广场与控制台模型配置深度分析文档

> 本文档深度分析项目前端的"模型广场"（Model Square）与控制台"模型"（Models）编辑页面之间的关系，详细说明控制台模型编辑页面的所有配置项含义、影响范围、配置示例，以及模型配置价格与真实调用价格的关系。

---

## 目录

1. [核心结论速览](#1-核心结论速览)
2. [架构关系总览](#2-架构关系总览)
3. [模型广场前端实现详解](#3-模型广场前端实现详解)
4. [控制台模型编辑页面所有配置项详解](#4-控制台模型编辑页面所有配置项详解)
5. [端点（Endpoint）配置深度解析](#5-端点endpoint配置深度解析)
6. [价格配置与真实调用价格的关系](#6-价格配置与真实调用价格的关系)
7. [哪些配置项影响模型广场展示](#7-哪些配置项影响模型广场展示)
8. [完整配置示例集锦](#8-完整配置示例集锦)

---

## 1. 核心结论速览

| 问题 | 答案 |
|------|------|
| 模型广场的数据来源？ | 后端 `/api/pricing` 接口，聚合了 `models` 表元数据 + `ratio_setting` 内存中的真实计费倍率 + `abilities` 表的启用分组 + 渠道端点类型 |
| 控制台模型编辑表单数据存储在哪？ | **元数据**存 `models` 表；**价格倍率**存 `options` 表（系统选项）的 JSON 字符串中 |
| 模型配置价格是否影响真实调用价格？ | **会影响**。表单中填写的 price/ratio/completionRatio 等会被写入 `options` 表对应的 `ModelPrice`/`ModelRatio`/`CompletionRatio` 等 JSON 配置，relay 转发时直接读取这些配置计算扣费金额 |
| models 表存不存计费倍率？ | **不存**。models 表只存元数据（描述、图标、标签、供应商、端点配置、状态、名称匹配规则等） |
| 端点配置影响什么？ | 影响（1）模型广场展示该模型支持哪些端点类型；（2）前端生成的 API 调用示例的 URL 路径；（3）实际请求转发的上游路径 |
| 修改 models 表的元数据会影响计费吗？ | **不会**（除修改 Endpoints 会改变上游请求路径外）。要修改真实计费必须通过 `ratio_setting` 写入 `options` 表 |

---

## 2. 架构关系总览

### 2.1 数据流图

```
┌───────────────────────────────────────────────────────────────────────┐
│                        管理员在控制台操作                                │
│  控制台 → 模型管理 → 编辑模型（抽屉表单）                                │
└────────────────────────┬──────────────────────────────────────────────┘
                         │
                         ▼
              ┌──────────────────────┐
              │  onSubmit 提交逻辑     │
              │  分离两部分数据        │
              └────┬─────────────┬─────┘
                   │             │
                   ▼             ▼
    ┌──────────────────────┐   ┌──────────────────────────────────────┐
    │  models 表写入        │   │  options 表写入（系统选项）            │
    │  - model_name         │   │  - ModelPrice:   JSON map[name]price │
    │  - description        │   │  - ModelRatio:    JSON map[name]ratio │
    │  - icon               │   │  - CompletionRatio                  │
    │  - tags               │   │  - CacheRatio                       │
    │  - vendor_id          │   │  - ImageRatio / AudioRatio 等        │
    │  - endpoints          │   └──────────────────────────────────────┘
    │  - name_rule          │                  │
    │  - status             │                  │
    │  - sync_official      │                  ▼
    └──────────────────────┘   ┌──────────────────────────────────────┐
                │               │  ratio_setting 内存映射              │
                │               │  启动时从 options 表加载到内存       │
                │               │  - modelRatioMap (RWMap 并发安全)   │
                │               │  - modelPriceMap                    │
                │               │  - completionRatioMap 等             │
                │               └────┬─────────────────────────────┘
                │                    │
                │                    │ 真实调用时直接读
                │                    ▼
                │     ┌──────────────────────────────────────────┐
                │     │  relay 转发链路                            │
                │     │  relay/helper/price.go::ModelPriceHelper  │
                │     │  → 预扣费 PreConsumeBilling               │
                │     │  → 转发请求到上游                          │
                │     │  → service/text_quota.go                  │
                │     │  → 最终扣费 SettleBilling                 │
                │     └──────────────────────────────────────────┘
                │
                ▼
    ┌────────────────────────────────────────────────────────────────┐
    │  model.Pricing 聚合视图（带 1 分钟缓存）                        │
    │  updatePricing() 整合三方面数据：                                │
    │    1. models 表（元数据）                                       │
    │    2. ratio_setting（真实计费倍率）                              │
    │    3. abilities + channels 表（启用分组、渠道端点类型）           │
    └────────────────────────┬───────────────────────────────────────┘
                            │
                            ▼
              ┌──────────────────────────────┐
              │  GET /api/pricing 接口        │
              │  返回给前端模型广场展示        │
              └──────────────────────────────┘
```

### 2.2 关键数据库表关系

| 表名 | 作用 | 是否影响真实计费 |
|------|------|-----------------|
| `models` | 模型元数据（描述、图标、标签、端点配置等） | 否（除 Endpoints 改变路径） |
| `options` | 系统选项 KV 存储，包含所有计费倍率 JSON | **是** |
| `abilities` | 渠道能力映射（模型在哪些渠道可用） | 影响**是否可用**，不影响价格 |
| `channels` | 渠道定义 | 影响默认端点类型推断 |
| `vendors` | 供应商信息 | 仅展示 |

### 2.3 关键代码文件索引

**前端**：
- 模型广场路由：`web/default/src/routes/pricing/index.tsx`
- 模型广场主组件：`web/default/src/features/pricing/index.tsx`
- 模型广场 API 调用：`web/default/src/features/pricing/api.ts`
- 模型广场类型定义：`web/default/src/features/pricing/types.ts`
- 模型广场价格计算：`web/default/src/features/pricing/lib/price.ts`
- 模型广场卡片视图：`web/default/src/features/pricing/components/model-card.tsx`
- 模型广场表格视图：`web/default/src/features/pricing/components/pricing-columns.tsx`
- 模型详情抽屉：`web/default/src/features/pricing/components/model-details.tsx`
- 模型详情 API 示例：`web/default/src/features/pricing/components/model-details-api.tsx`

**控制台模型管理**：
- 模型管理路由：`web/default/src/routes/_authenticated/models/$section.tsx`
- 模型管理主页面：`web/default/src/features/models/index.tsx`
- **模型编辑抽屉（核心表单）**：`web/default/src/features/models/components/drawers/model-mutate-drawer.tsx`
- 模型列表表格：`web/default/src/features/models/components/models-table.tsx`
- 常量定义（端点模板等）：`web/default/src/features/models/constants.ts`
- 类型定义：`web/default/src/features/models/types.ts`
- 表单 Schema 与工具函数：`web/default/src/features/models/lib/model-form.ts`

**后端**：
- 模型元数据结构：`model/model_meta.go`
- 价格聚合逻辑：`model/pricing.go`
- 价格刷新机制：`model/pricing_refresh.go`
- 模型附加缓存：`model/model_extra.go`
- Pricing Controller：`controller/pricing.go`
- Model Meta Controller：`controller/model_meta.go`
- 真实计费入口：`relay/helper/price.go`
- 真实扣费计算：`service/text_quota.go`
- 倍率设置内存映射：`setting/ratio_setting/model_ratio.go`
- 端点默认配置：`common/endpoint_defaults.go`
- 端点类型推断：`common/endpoint_type.go`
- EndpointType 常量：`constant/endpoint_type.go`
- 计费表达式（分层计费）：`pkg/billingexpr/`

---

## 3. 模型广场前端实现详解

### 3.1 路由与入口

**路由文件**：`web/default/src/routes/pricing/index.tsx`

```tsx
const pricingSearchSchema = z.object({
  search: z.string().optional(),
  sort: z.string().optional(),
  vendor: z.string().optional(),
  group: z.string().optional(),
  quotaType: z.string().optional(),
  endpointType: z.string().optional(),
  tag: z.string().optional(),
  tokenUnit: z.enum(['M', 'K']).optional(),
  view: z.enum(['card', 'table']).optional().catch(undefined),
  rechargePrice: z.boolean().optional(),
})

export const Route = createFileRoute('/pricing/')({
  validateSearch: pricingSearchSchema,
  beforeLoad: async ({ location }) => {
    const access = await getFreshModuleAccess('pricing')
    if (!access.enabled) {
      throw redirect({ to: '/' })
    }
    // ... 登录校验
  },
  component: Pricing,
})
```

**导航入口**：`web/default/src/hooks/use-top-nav-links.ts` 第 74-79 行

```ts
const pricing = modules?.pricing
if (pricing && typeof pricing === 'object' && pricing.enabled) {
  const requiresAuth = pricing.requireAuth && !isAuthed
  links.push({ title: t('Model Square'), href: '/pricing', requiresAuth })
}
```

- "Model Square" 在 `zh.json` 中翻译为 "模型广场"
- 入口显示由后端 `/api/status` 返回的 `HeaderNavModules.pricing = { enabled, requireAuth }` 控制

### 3.2 数据获取接口

**前端 API**：`web/default/src/features/pricing/api.ts`

```ts
export async function getPricing(): Promise<PricingData> {
  const res = await api.get('/api/pricing')
  return res.data
}
```

**响应结构（前端类型）**：`web/default/src/features/pricing/types.ts`

```ts
export type PricingData = {
  success: boolean
  message?: string
  data: PricingModel[]                 // 模型列表
  vendors: PricingVendor[]            // 供应商列表
  group_ratio: Record<string, number> // 分组倍率
  usable_group: Record<string, { desc: string; ratio: number }> // 用户可用分组
  supported_endpoint: Record<string, string>  // 端点 → 路径
  auto_groups: string[]              // 自动分组链
}
```

**后端响应结构**：`controller/pricing.go` 第 67-77 行

```go
c.JSON(200, gin.H{
    "success":            true,
    "data":               pricing,                          // []model.Pricing
    "vendors":            model.GetVendors(),                // []PricingVendor
    "group_ratio":        groupRatio,                        // map[string]float64
    "usable_group":       usableGroup,                       // map[string]string (desc)
    "supported_endpoint": model.GetSupportedEndpointMap(),   // map[string]common.EndpointInfo
    "auto_groups":        service.GetUserAutoGroup(group),   // []string
    "pricing_version":    "a42d372ccf0b5dd13ecf71203521f9d2",
})
```

**Pricing 单条记录结构**：`model/pricing.go` 第 18-39 行

```go
type Pricing struct {
    ModelName              string                  `json:"model_name"`
    Description            string                  `json:"description,omitempty"`
    Icon                   string                  `json:"icon,omitempty"`
    Tags                   string                  `json:"tags,omitempty"`
    VendorID               int                     `json:"vendor_id,omitempty"`
    QuotaType              int                     `json:"quota_type"`                  // 0=按token, 1=按次
    ModelRatio             float64                 `json:"model_ratio"`                 // 模型倍率
    ModelPrice             float64                 `json:"model_price"`                 // 按次价格
    OwnerBy                string                  `json:"owner_by"`
    CompletionRatio        float64                 `json:"completion_ratio"`
    CacheRatio             *float64                `json:"cache_ratio,omitempty"`
    CreateCacheRatio       *float64                `json:"create_cache_ratio,omitempty"`
    ImageRatio             *float64                `json:"image_ratio,omitempty"`
    AudioRatio             *float64                `json:"audio_ratio,omitempty"`
    AudioCompletionRatio   *float64                `json:"audio_completion_ratio,omitempty"`
    EnableGroup            []string                `json:"enable_groups"`
    SupportedEndpointTypes []constant.EndpointType `json:"supported_endpoint_types"`
    BillingMode            string                  `json:"billing_mode,omitempty"`       // tiered_expr
    BillingExpr            string                  `json:"billing_expr,omitempty"`
    PricingVersion         string                  `json:"pricing_version,omitempty"`
}
```

### 3.3 模型广场展示的字段

#### 主列表展示（卡片 + 表格）

| 展示字段 | 数据来源 | 展示位置 |
|---------|---------|---------|
| 模型名称 | `Pricing.ModelName` | 卡片标题 / 表格 Model 列 |
| 图标 | `Pricing.Icon` + `Vendor.Icon` | 卡片左侧 / 表格 Model 列 |
| 描述 | `Pricing.Description` | 卡片描述行（line-clamp-2） |
| 计费类型 | `Pricing.QuotaType` | 卡片底部标签 / 表格 Type 列 |
| Input 价格 | `Pricing.ModelRatio` × 2 × group_ratio | 卡片价格行 / 表格 Price 列 |
| Output 价格 | Input × `Pricing.CompletionRatio` | 同上 |
| 缓存命中价格 | Input × `Pricing.CacheRatio` | 卡片 Cached / 表格 Cached 列 |
| 按次价格 | `Pricing.ModelPrice` | 按次计费模型展示 |
| 启用分组 | `Pricing.EnableGroup` | 卡片底部 + 表格 Groups 列 |
| 标签 | `Pricing.Tags`（逗号分隔字符串） | 卡片底部前 2 个 / 表格 Tags 列 |
| 端点类型 | `Pricing.SupportedEndpointTypes` | 卡片底部前 2 个 / 表格 Endpoints 列 |
| 供应商 | `Vendor.Name` + `Vendor.Icon` | 表格 Vendor 列 |
| 动态定价徽章 | `Pricing.BillingMode == 'tiered_expr'` | 卡片徽章 / 表格 Price 列 |
| 性能指标 | `getPerfMetricsSummary(24)` 独立查询 | 卡片右下角徽章 |

#### 模型详情抽屉额外展示

- **Header**：模型名（可复制）、供应商、计费类型、描述、动态定价徽章
- **Overview Tab**：
  - 性能摘要：TPS、平均延迟、成功率
  - 元数据：context_length、max_output_tokens、modalities、knowledge_cutoff 等（注：当前后端未填充这些字段，展示为空）
  - **PriceSection**：Input/Output 主价格 + Cached input / Cache write / Image input / Audio input / Audio output 二级价格
  - **GroupPricingSection**：按可用分组展开的价格表
  - 能力列表（capabilities）
- **API Tab**：
  - 根据 `supported_endpoint_types` 和 `endpointMap` 生成 cURL / Python / TypeScript / JavaScript 代码示例
  - Supported parameters 表
  - Rate limits 表

### 3.4 过滤、排序、分类逻辑

**过滤链**（`web/default/src/features/pricing/lib/filters.ts`）：

```ts
export function filterAndSortModels(models, filters) {
  let result = filterBySearch(models, filters.search)         // 1. 搜索（model_name + description + tags + vendor_name）
  result = filterByVendor(result, filters.vendor)              // 2. 供应商精确匹配
  result = filterByGroup(result, filters.group)                // 3. 分组（enable_groups.includes）
  result = filterByQuotaType(result, filters.quotaType)        // 4. 计费类型（token=0, request=1）
  result = filterByEndpointType(result, filters.endpointType) // 5. 端点类型
  result = filterByTag(result, filters.tag)                   // 6. 标签
  result = sortModels(result, filters.sortBy)                 // 7. 排序
  return result
}
```

**排序选项**：
- `name`（默认）：按 `model_name` 的 `localeCompare` 升序
- `price-low`：按价格升序
- `price-high`：按价格降序

**标签解析规则**：
```ts
return tagsString.split(/[,;|\s]+/).map((t) => t.trim()).filter(Boolean)
```
支持逗号、分号、竖线、空格四种分隔符。

### 3.5 价格展示计算逻辑

**前端价格计算**（`web/default/src/features/pricing/lib/price.ts`）：

```ts
// 按量计费（token）模型的价格计算
function calculateTokenPrice(model, groupRatio, tokenUnit) {
  const base = model.model_ratio * 2 * groupRatio  // 注意 ×2
  return {
    input: base,
    output: base * model.completion_ratio,
    cache: base * model.cache_ratio,
    create_cache: base * model.create_cache_ratio,
    image: base * model.image_ratio,
    audio_input: base * model.audio_ratio,
    audio_output: base * model.audio_ratio * model.audio_completion_ratio,
  }
}

// 按次计费模型
function calculateRequestPrice(model, groupRatio) {
  return model.model_price * groupRatio
}
```

**关键换算关系**：
- `1 倍率 = $0.002 / 1K tokens = $2 / 1M tokens`
- `1 USD = 500000 Quota`（`common.QuotaPerUnit = 500 * 1000.0`）

---

## 4. 控制台模型编辑页面所有配置项详解

### 4.1 表单 Schema

表单组件位于 `web/default/src/features/models/components/drawers/model-mutate-drawer.tsx`，使用 `react-hook-form` + `zod`。

```tsx
const extendedModelFormSchema = z.object({
  id: z.number().optional(),
  model_name: z.string().min(1, 'Model name is required'),
  description: z.string(),
  icon: z.string(),
  tags: z.array(z.string()),
  vendor_id: z.number().optional(),
  endpoints: z.string(),
  name_rule: z.number(),
  status: z.boolean(),
  sync_official: z.boolean(),
  price: z.string().optional(),
  ratio: z.string().optional(),
  cacheRatio: z.string().optional(),
  completionRatio: z.string().optional(),
  imageRatio: z.string().optional(),
  audioRatio: z.string().optional(),
  audioCompletionRatio: z.string().optional(),
})
```

### 4.2 表单分区与字段完整清单

表单分为 5 个区块：

#### A. 基础信息（Basic Information）

| # | 字段名 | 类型 | 含义 | 必填 | 默认值 | 影响模型广场 | 影响真实计费 |
|---|--------|------|------|------|--------|------------|-------------|
| 1 | `model_name` | string | 模型唯一标识符 | 是 | - | ✅ 展示名称 | ✅ 作为计费 key | 
| 2 | `description` | string | 模型描述 | 否 | `''` | ✅ 卡片描述 | ❌ |
| 3 | `icon` | string | 图标 key（@lobehub/icons） | 否 | `''` | ✅ 卡片/表格图标 | ❌ |
| 4 | `vendor_id` | number | 供应商 ID（外键） | 否 | undefined | ✅ 关联供应商名/图标 | ❌ |
| 5 | `tags` | string[] | 标签数组 | 否 | `[]` | ✅ 卡片底部标签 | ❌ |

#### B. 匹配规则（Matching Rules）

| # | 字段名 | 类型 | 含义 | 默认值 | 影响模型广场 | 影响真实计费 |
|---|--------|------|------|--------|------------|-------------|
| 6 | `name_rule` | number (0-3) | 名称匹配规则 | 0（精确） | ✅ 影响哪些模型被该元数据匹配 | ❌ |

**name_rule 取值**：

| 值 | 名称 | 含义 | 模型广场影响 |
|----|------|------|-------------|
| 0 | Exact Match（精确匹配） | 只匹配完全同名的模型 | 仅该模型显示此元数据 |
| 1 | Prefix Match（前缀匹配） | 匹配所有以此名开头的模型 | 所有匹配的模型共享此元数据 |
| 2 | Contains Match（包含匹配） | 匹配所有包含此名的模型 | 同上 |
| 3 | Suffix Match（后缀匹配） | 匹配所有以此名结尾的模型 | 同上 |

#### C. 端点配置（Endpoints）

| # | 字段名 | 类型 | 含义 | 默认值 | 影响模型广场 | 影响真实计费 |
|---|--------|------|------|--------|------------|-------------|
| 7 | `endpoints` | string (JSON) | 端点配置 JSON | `''` | ✅ 支持的端点类型 | ✅ 改变上游请求路径 |

详见第 5 章。

#### D. 价格配置（Pricing Configuration）

| # | 字段名 | 类型 | 含义 | 系统选项 key | 影响模型广场 | 影响真实计费 |
|---|--------|------|------|-------------|------------|-------------|
| 8 | `price` | string | 按次价格（USD） | `ModelPrice` | ✅ 按次价格展示 | ✅ **直接决定扣费金额** |
| 9 | `ratio` | string | 模型倍率 | `ModelRatio` | ✅ Input 价格展示 | ✅ **直接决定扣费金额** |
| 10 | `completionRatio` | string | 完成 token 倍率 | `CompletionRatio` | ✅ Output 价格展示 | ✅ **直接决定扣费金额** |
| 11 | `cacheRatio` | string | 缓存命中倍率 | `CacheRatio` | ✅ Cached 价格 | ✅ **直接决定扣费金额** |
| 12 | `imageRatio` | string | 图像处理倍率 | `ImageRatio` | ✅ Image 价格（详情） | ✅ **直接决定扣费金额** |
| 13 | `audioRatio` | string | 音频输入倍率 | `AudioRatio` | ✅ Audio input 价格 | ✅ **直接决定扣费金额** |
| 14 | `audioCompletionRatio` | string | 音频输出倍率 | `AudioCompletionRatio` | ✅ Audio output 价格 | ✅ **直接决定扣费金额** |

**价格模式说明**：

表单提供两种价格模式（`pricingMode` 状态变量）：

1. **per-token（按 token 计费）**：基于倍率系统
   - 子模式 `ratio`：直接输入倍率数值
   - 子模式 `price`：输入 USD/1M tokens，自动换算为倍率（`倍率 = 价格 / 2`）
2. **per-request（按请求计费）**：固定价格（USD/次）

**换算公式**：
| 换算方向 | 公式 |
|---------|------|
| ratio → USD/1M tokens | `价格 = 倍率 × 2` |
| USD/1M tokens → ratio | `倍率 = 价格 / 2` |
| completionRatio → completion USD/1M | `完成价 = prompt价 × completionRatio` |
| completionPrice → completionRatio | `completionRatio = completionPrice / promptPrice` |

#### E. 状态与同步（Status & Sync）

| # | 字段名 | 类型 | 含义 | 默认值 | 影响模型广场 | 影响真实计费 |
|---|--------|------|------|--------|------------|-------------|
| 15 | `status` | boolean | 启用状态 | true | ✅ false 时模型不展示 | ❌（但 status=false 模型不展示后用户无法调用） |
| 16 | `sync_official` | boolean | 是否同步官方上游 | true | ❌ | ❌（仅作为标记位） |

### 4.3 提交时的数据分离逻辑

`onSubmit` 函数（第 411-614 行）将表单数据分离为两部分：

```tsx
const submitData = {
  ...values,
  id: isEditing ? currentModelId : undefined,
  tags: Array.isArray(values.tags) ? values.tags.join(',') : '',  // 数组转逗号字符串
  status: values.status ? 1 : 0,                                     // 布尔转 0/1
  sync_official: values.sync_official ? 1 : 0,
}

// 分离价格字段（它们存储在系统选项中，不存入 models 表）
const {
  price, ratio, cacheRatio, completionRatio,
  imageRatio, audioRatio, audioCompletionRatio,
  ...modelData  // 这部分写入 models 表
} = submitData

// 1. 写入 models 表
await updateModel({ ...modelData, id: currentModelId })

// 2. 写入 options 表（系统选项 JSON）
//  - 先删除旧模型名条目（若改名）
//  - 按模式重新写入对应 map
//  - 批量调用 updateOption 更新变化的系统选项
```

### 4.4 后端 models 表结构

**数据库表结构**（`model/model_meta.go` 第 24-45 行）：

```go
type Model struct {
    Id           int            `json:"id"`
    ModelName    string         `json:"model_name" gorm:"size:128;not null;uniqueIndex:uk_model_name_delete_at,priority:1"`
    Description  string         `json:"description,omitempty" gorm:"type:text"`
    Icon         string         `json:"icon,omitempty" gorm:"type:varchar(128)"`
    Tags         string         `json:"tags,omitempty" gorm:"type:varchar(255)"`          // 逗号分隔字符串
    VendorID     int            `json:"vendor_id,omitempty" gorm:"index"`
    Endpoints    string         `json:"endpoints,omitempty" gorm:"type:text"`           // JSON 文本
    Status       int            `json:"status" gorm:"default:1"`                        // 1=启用, 0=禁用
    SyncOfficial int            `json:"sync_official" gorm:"default:1"`
    CreatedTime  int64          `json:"created_time" gorm:"bigint"`
    UpdatedTime  int64          `json:"updated_time" gorm:"bigint"`
    DeletedAt    gorm.DeletedAt `json:"-" gorm:"index;uniqueIndex:uk_model_name_delete_at,priority:2"`

    // 以下字段不入库（gorm:"-"），由 Controller enrichModels 动态填充
    BoundChannels []BoundChannel `json:"bound_channels,omitempty" gorm:"-"`
    EnableGroups  []string       `json:"enable_groups,omitempty" gorm:"-"`
    QuotaTypes    []int          `json:"quota_types,omitempty" gorm:"-"`
    NameRule      int            `json:"name_rule" gorm:"default:0"`
    MatchedModels []string       `json:"matched_models,omitempty" gorm:"-"`
    MatchedCount  int           `json:"matched_count,omitempty" gorm:"-"`
}
```

**关键约束**：
- `(model_name, deleted_at)` 联合唯一索引，软删除后可重用同名
- 后端 Update 使用 `Select` 强制更新所有字段（包括零值）
- `gorm:"-"` 标记的字段不持久化，由 `enrichModels` 动态填充

---

## 5. 端点（Endpoint）配置深度解析

### 5.1 端点类型定义

**后端常量**（`constant/endpoint_type.go`）：

```go
type EndpointType string

const (
    EndpointTypeOpenAI                EndpointType = "openai"                  // OpenAI Chat Completions
    EndpointTypeOpenAIResponse        EndpointType = "openai-response"          // OpenAI Responses API
    EndpointTypeOpenAIResponseCompact EndpointType = "openai-response-compact" // OpenAI Responses Compact
    EndpointTypeAnthropic             EndpointType = "anthropic"               // Anthropic Messages
    EndpointTypeGemini                EndpointType = "gemini"                  // Gemini generateContent
    EndpointTypeJinaRerank            EndpointType = "jina-rerank"             // Jina Rerank
    EndpointTypeImageGeneration       EndpointType = "image-generation"         // 图像生成
    EndpointTypeEmbeddings            EndpointType = "embeddings"               // 文本嵌入
    EndpointTypeOpenAIVideo           EndpointType = "openai-video"             // 视频生成
)
```

### 5.2 默认端点路径映射

**后端默认路径**（`common/endpoint_defaults.go`）：

```go
var defaultEndpointInfoMap = map[constant.EndpointType]EndpointInfo{
    constant.EndpointTypeOpenAI:                {Path: "/v1/chat/completions", Method: "POST"},
    constant.EndpointTypeOpenAIResponse:        {Path: "/v1/responses", Method: "POST"},
    constant.EndpointTypeOpenAIResponseCompact: {Path: "/v1/responses/compact", Method: "POST"},
    constant.EndpointTypeAnthropic:             {Path: "/v1/messages", Method: "POST"},
    constant.EndpointTypeGemini:                {Path: "/v1beta/models/{model}:generateContent", Method: "POST"},
    constant.EndpointTypeJinaRerank:            {Path: "/v1/rerank", Method: "POST"},
    constant.EndpointTypeImageGeneration:       {Path: "/v1/images/generations", Method: "POST"},
    constant.EndpointTypeEmbeddings:            {Path: "/v1/embeddings", Method: "POST"},
}
```

### 5.3 端点类型自动推断（基于渠道类型）

`common/endpoint_type.go::GetEndpointTypesByChannelType` 根据渠道类型自动推断端点：

| 渠道类型 | 推断的端点类型 | 说明 |
|---------|---------------|------|
| Anthropic / AWS Bedrock | `[anthropic, openai]` | Anthropic 优先，OpenAI 兼容 |
| Vertex AI / Gemini | `[gemini, openai]` | Gemini 优先 |
| OpenRouter | `[openai]` | 仅 OpenAI |
| XAI | `[openai, openai-response]` | 支持 Responses API |
| Sora | `[openai-video]` | 视频生成 |
| Jina | `[jina-rerank]` | Rerank |
| 默认（OpenAI 等） | `[openai]` 或 `[openai-response]`（Responses-only 模型） | |
| 图像生成模型 | 在推断结果前插入 `image-generation` | |

### 5.4 端点配置的两层来源与合并

`model/pricing.go::updatePricing()` 中端点配置有两层来源：

1. **默认端点**（基于渠道类型自动推断）：从 `abilities + channels` 表查询
2. **自定义端点**（models 表 `endpoints` JSON 字段）：**会覆盖默认端点**（不做合并）

**合并逻辑**：
```go
// 1. 先按渠道能力填充默认端点
modelSupportEndpointsStr := getDefaultEndpointsForModel(modelName, channelTypes)

// 2. 如果 models 表中有自定义 endpoints，覆盖默认值
if mm.Endpoints != "" {
    customEndpoints := parseJSON(mm.Endpoints)
    // 自定义端点覆盖默认端点
    supportedEndpointMap = mergeEndpoints(defaultEndpoints, customEndpoints)
}
```

### 5.5 endpoints 字段数据结构

`endpoints` 字段是 JSON 字符串，结构为 `Record<endpoint_type, EndpointInfo | string>`：

```json
{
  "openai": { "path": "/v1/chat/completions", "method": "POST" },
  "anthropic": { "path": "/v1/messages", "method": "POST" }
}
```

**支持的 value 格式**：
- **对象格式**（推荐）：`{ "path": "/v1/...", "method": "POST" }`
- **字符串格式**（兼容）：`"/v1/chat/completions"`（method 默认 POST）

### 5.6 端点模板

**前端常量**（`web/default/src/features/models/constants.ts` 第 158-169 行）：

```tsx
export const ENDPOINT_TEMPLATES: Record<
  string,
  { path: string; method: string }
> = {
  openai: { path: '/v1/chat/completions', method: 'POST' },
  'openai-response': { path: '/v1/responses', method: 'POST' },
  anthropic: { path: '/v1/messages', method: 'POST' },
  gemini: { path: '/v1beta/models/{model}:generateContent', method: 'POST' },
  'jina-rerank': { path: '/rerank', method: 'POST' },
  'image-generation': { path: '/v1/images/generations', method: 'POST' },
  embeddings: { path: '/v1/embeddings', method: 'POST' },
}
```

**模板加载逻辑**：选择模板时会用 `{ [templateKey]: template }` 包装后写入 `endpoints` 字段（即只填入单个端点配置）。

### 5.7 端点配置如何影响调用

1. **模型广场展示**：`SupportedEndpointTypes` 决定模型广场显示该模型支持哪些端点类型
2. **前端 API 示例生成**：`supported_endpoint` 全局映射决定每种端点类型对应的 URL 路径，用于生成 cURL/Python 代码示例
3. **实际请求转发**：端点配置影响上游请求路径（自定义端点会覆盖默认端点）
4. **Controller enrichModels**：
   - 精确匹配模型：从缓存 `model.GetModelSupportEndpointTypes(modelName)` 读取
   - 规则匹配模型（prefix/suffix/contains）：聚合所有匹配模型的 `SupportedEndpointTypes` 并集

### 5.8 端点配置的模型广场影响

**对精确匹配模型（name_rule=0）**：
- 自定义 endpoints 完全覆盖默认端点
- 模型广场展示自定义的端点类型列表

**对规则匹配模型（name_rule=1/2/3）**：
- 后端会聚合所有匹配到的真实模型的端点类型并集
- 自定义 endpoints 会作为这些模型的端点来源之一

---

## 6. 价格配置与真实调用价格的关系

### 6.1 关键结论

**模型编辑表单中的价格配置会直接影响真实调用价格。**

表单中填写的 price/ratio/completionRatio 等字段会被写入 `options` 表的 JSON 字符串中，relay 转发时直接读取这些配置计算扣费金额。

### 6.2 价格数据存储与读取流程

```
管理员填写表单
    ↓
onSubmit 分离价格字段
    ↓
调用 updateOption 更新系统选项：
  - ModelPrice:    {"gpt-4": 0.01, "claude-3-opus": 0.05}
  - ModelRatio:    {"gpt-4": 15, "gpt-4o": 1.25}
  - CompletionRatio: {"gpt-4": 2, "claude-3-opus": 5}
  - CacheRatio, ImageRatio, AudioRatio 等
    ↓
options 表更新
    ↓
ratio_setting.UpdateModelRatioByJSONString 等函数同步更新内存映射
    ↓
relay 转发时直接从内存映射读取
```

### 6.3 真实计费的代码路径

**预扣费**（`relay/helper/price.go::ModelPriceHelper`）：

```go
func ModelPriceHelper(c *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *types.TokenCountMeta) (types.PriceData, error) {
    // 1. 检查是否按次计费
    modelPrice, usePrice := ratio_setting.GetModelPrice(info.OriginModelName, false)
    
    // 2. 检查是否分层计费
    if billing_setting.GetBillingMode(info.OriginModelName) == billing_setting.BillingModeTieredExpr {
        return modelPriceHelperTiered(c, info, promptTokens, meta, groupRatioInfo)
    }
    
    if !usePrice {
        // 按量计费（token）
        modelRatio, success, matchName = ratio_setting.GetModelRatio(info.OriginModelName)
        if !success {
            return types.PriceData{}, modelPriceNotConfiguredError(matchName, info.UserId)
        }
        completionRatio = ratio_setting.GetCompletionRatio(info.OriginModelName)
        cacheRatio, _ = ratio_setting.GetCacheRatio(info.OriginModelName)
        cacheCreationRatio, _ = ratio_setting.GetCreateCacheRatio(info.OriginModelName)
        imageRatio, _ = ratio_setting.GetImageRatio(info.OriginModelName)
        audioRatio = ratio_setting.GetAudioRatio(info.OriginModelName)
        audioCompletionRatio = ratio_setting.GetAudioCompletionRatio(info.OriginModelName)
        
        ratio := modelRatio * groupRatioInfo.GroupRatio
        preConsumedQuota = int(float64(preConsumedTokens) * ratio)
    } else {
        // 按次计费
        preConsumedQuota = int(modelPrice * common.QuotaPerUnit * groupRatioInfo.GroupRatio)
    }
}
```

**最终扣费**（`service/text_quota.go::calculateTextQuotaSummary`）：

按量计费核心公式：
```go
ratio := modelRatio * groupRatio

// 基础 token = prompt - cache - image - cachedCreation（OpenAI 语义）
// 或 prompt（Claude 语义，不减去 cache）
baseTokens = promptTokens - cacheTokens - imageTokens - cachedCreationTokens

promptQuota = baseTokens
            + cacheTokens * cacheRatio
            + cachedCreationTokens * cacheCreationRatio
            + imageTokens * imageRatio

completionQuota = completionTokens * completionRatio

quota = (promptQuota + completionQuota) * ratio
       + toolCallSurchargeQuota    // web_search 等附加费
       + audioInputQuota

if quota <= 0 && ratio != 0 { quota = 1 }  // 最小扣 1
```

按次计费公式：
```go
quota = modelPrice * QuotaPerUnit * groupRatio + toolCallSurcharge + audioInputQuota
```

### 6.4 关键常量

**`setting/ratio_setting/model_ratio.go` 第 13-16 行**：

```go
const (
    USD2RMB = 7.3            // 1 USD = 7.3 RMB
    USD     = 500            // $0.002 = 1 倍率 → $1 = 500
    RMB     = USD / USD2RMB  // 1 RMB ≈ 68.49
)
```

**`common/constants.go`**：
- `QuotaPerUnit = 500 * 1000.0`（即 $1 = 500000 Quota）
- `PreConsumedQuota = 500`（预扣 token 默认值）

### 6.5 倍率与价格换算示例

| 倍率 | USD/1K tokens | USD/1M tokens | RMB/1K tokens |
|------|---------------|---------------|---------------|
| 0.075 | $0.00015 | $0.15 | ¥0.0011 |
| 0.25 | $0.0005 | $0.5 | ¥0.0037 |
| 1.0 | $0.002 | $2 | ¥0.0146 |
| 1.25 | $0.0025 | $2.5 | ¥0.0183 |
| 2.5 | $0.005 | $5 | ¥0.0365 |
| 5 | $0.01 | $10 | ¥0.073 |
| 15 | $0.03 | $30 | ¥0.219 |
| 75 | $0.15 | $150 | ¥1.095 |

**换算公式**：
- `USD/1M tokens = 倍率 × 2`
- `USD/1K tokens = 倍率 × 0.002`
- `RMB/1M tokens = 倍率 × 2 × 7.3`

### 6.6 默认倍率（部分）

后端内置了部分模型的默认倍率（`setting/ratio_setting/model_ratio.go` 第 26-340 行的 `defaultModelRatio`）：

```go
var defaultModelRatio = map[string]float64{
    "gpt-4":              15,    // $30 / 1M tokens
    "gpt-4o":             1.25,  // $2.5 / 1M tokens
    "gpt-4o-mini":        0.075, // $0.15 / 1M tokens
    "gpt-3.5-turbo":      0.25,  // $0.5 / 1M tokens
    "gpt-4.1":            1.0,   // $2 / 1M tokens
    "gpt-4.1-mini":       0.2,   // $0.4 / 1M tokens
    "o1":                 7.5,   // $15 / 1M tokens
    "o3-mini":            0.55,  // $1.1 / 1M tokens
    "o3-pro":             10.0,  // $20 / 1M tokens
    // ... 更多模型
}
```

### 6.7 GetModelRatio 未配置时的行为

**`setting/ratio_setting/model_ratio.go::GetModelRatio` 第 392-406 行**：

```go
func GetModelRatio(name string) (float64, bool, string) {
    name = FormatMatchingModelName(name)
    
    ratio, ok := modelRatioMap.Get(name)
    if !ok {
        if strings.HasSuffix(name, CompactModelSuffix) {
            if wildcardRatio, ok := modelRatioMap.Get(CompactWildcardModelKey); ok {
                return wildcardRatio, true, name
            }
        }
        return 37.5, operation_setting.SelfUseModeEnabled, name  // 默认 37.5 倍率
    }
    return ratio, true, name
}
```

**未配置模型倍率时的默认行为**：
- 返回 `37.5`（即 $75 / 1M tokens，高价防止误用）
- `success = operation_setting.SelfUseModeEnabled`：仅在自用模式下允许调用
- 非自用模式下返回 `false`，触发 `modelPriceNotConfiguredError`

---

## 7. 哪些配置项影响模型广场展示

### 7.1 影响模型广场展示的配置项

| 配置项 | 影响模型广场的方式 |
|--------|------------------|
| `model_name` | ✅ 模型名称展示；作为计费 key；唯一标识符 |
| `description` | ✅ 卡片描述行（line-clamp-2 截断） |
| `icon` | ✅ 卡片左侧图标（@lobehub/icons key） |
| `vendor_id` | ✅ 关联供应商名/图标展示 |
| `tags` | ✅ 卡片底部前 2 个标签；侧边栏过滤器；表格 Tags 列 |
| `endpoints` | ✅ 自定义端点配置覆盖默认端点；展示支持的端点类型 |
| `name_rule` | ✅ 影响元数据匹配哪些模型（精确/前缀/包含/后缀） |
| `status` | ✅ false 时该模型不展示在模型广场 |
| `price` | ✅ 按次计费模型的价格展示 |
| `ratio` | ✅ Input 价格 = 倍率 × 2 × group_ratio |
| `completionRatio` | ✅ Output 价格 = Input × completionRatio |
| `cacheRatio` | ✅ Cached 价格展示 |
| `imageRatio` | ✅ 详情抽屉的 Image input 价格 |
| `audioRatio` | ✅ 详情抽屉的 Audio input 价格 |
| `audioCompletionRatio` | ✅ 详情抽屉的 Audio output 价格 |
| `sync_official` | ❌ 不直接影响展示（仅作为标记位） |

### 7.2 不影响模型广场展示的配置项

| 配置项 | 说明 |
|--------|------|
| `sync_official` | 仅作为是否同步官方的标记，不影响展示 |
| `id` | 内部主键，不展示 |

### 7.3 后端 enrichModels 的动态填充字段

以下字段由后端 `controller/model_meta.go::enrichModels` 动态填充，不在编辑表单中：

| 字段 | 含义 | 数据来源 |
|------|------|---------|
| `bound_channels` | 绑定的渠道列表 | 查询 abilities + channels 表 |
| `enable_groups` | 启用的分组列表 | 查询 abilities 表 |
| `quota_types` | 计费类型集合 | 从 ratio_setting 推断 |
| `matched_models` | 匹配到的模型名 | 规则匹配时计算 |
| `matched_count` | 匹配数量 | 规则匹配时计算 |
| `created_time` | 创建时间戳 | models 表 |
| `updated_time` | 更新时间戳 | models 表 |

### 7.4 模型广场的过滤维度

模型广场支持以下过滤维度（对应 URL search 参数）：

| 过滤器 | URL 参数 | 取值 | 数据来源 |
|--------|---------|------|---------|
| 搜索 | `search` | 关键词 | 匹配 model_name + description + tags + vendor_name |
| 供应商 | `vendor` | 供应商名 | `PricingVendor.Name` |
| 分组 | `group` | 分组名 | `Pricing.EnableGroup` |
| 计费类型 | `quotaType` | `token` / `request` | `Pricing.QuotaType`（0/1） |
| 端点类型 | `endpointType` | 端点类型 | `Pricing.SupportedEndpointTypes` |
| 标签 | `tag` | 标签名 | `Pricing.Tags`（解析后匹配） |
| 排序 | `sort` | `name` / `price-low` / `price-high` | 按模型名或价格 |
| Token 单位 | `tokenUnit` | `M` / `K` | 仅影响展示 |
| 视图模式 | `view` | `card` / `table` | 仅影响展示 |
| 价格模式 | `rechargePrice` | boolean | 充值价 vs 真实价 |

---

## 8. 完整配置示例集锦

### 8.1 OpenAI GPT-4o 配置示例

**基础信息**：
- Model Name: `gpt-4o`
- Description: `GPT-4o 是 OpenAI 的旗舰多模态模型，支持文本、图像、音频输入`
- Icon: `OpenAI`
- Vendor: OpenAI
- Tags: `["chat", "vision", "multimodal"]`

**匹配规则**：Exact Match（精确匹配）

**端点配置**（JSON）：
```json
{
  "openai": {
    "path": "/v1/chat/completions",
    "method": "POST"
  }
}
```

**价格配置**：
- Pricing mode: `per-token`
- Input mode: `price`（USD/1M tokens）
- Prompt price: `$2.5`
- Completion price: `$10`
- 高级选项（展开）：
  - Cache ratio: `0.5`（缓存命中半价）

**实际写入系统选项**：
```json
// ModelRatio
{
  "gpt-4o": 1.25
}
// CompletionRatio
{
  "gpt-4o": 4
}
// CacheRatio
{
  "gpt-4o": 0.5
}
```

### 8.2 Claude 3.5 Sonnet 配置示例

**基础信息**：
- Model Name: `claude-3-5-sonnet-20241022`
- Description: `Anthropic Claude 3.5 Sonnet，擅长分析和编码`
- Icon: `Anthropic`
- Vendor: Anthropic
- Tags: `["chat", "vision", "long-context"]`

**匹配规则**：Prefix Match（前缀匹配，匹配 `claude-3-5-sonnet-*`）

**端点配置**（同时支持 Anthropic 和 OpenAI 格式）：
```json
{
  "anthropic": {
    "path": "/v1/messages",
    "method": "POST"
  },
  "openai": {
    "path": "/v1/chat/completions",
    "method": "POST"
  }
}
```

**价格配置**：
- Pricing mode: `per-token`
- Input mode: `ratio`（直接倍率）
- Model ratio: `1.0`
- Completion ratio: `5`

### 8.3 Gemini 1.5 Pro 配置示例

**端点配置**（Gemini 原生格式，含 {model} 占位符）：
```json
{
  "gemini": {
    "path": "/v1beta/models/{model}:generateContent",
    "method": "POST"
  },
  "openai": {
    "path": "/v1/chat/completions",
    "method": "POST"
  }
}
```

### 8.4 按次计费模型示例（图像生成）

**基础信息**：
- Model Name: `dall-e-3`
- Description: `DALL-E 3 图像生成模型`
- Tags: `["image", "generation"]`

**端点配置**：
```json
{
  "image-generation": {
    "path": "/v1/images/generations",
    "method": "POST"
  }
}
```

**价格配置**：
- Pricing mode: `per-request`
- Fixed price: `$0.04`（每次请求 0.04 美元）

**实际写入系统选项**：
```json
// ModelPrice
{
  "dall-e-3": 0.04
}
```

### 8.5 Embedding 模型示例

**端点配置**：
```json
{
  "embeddings": {
    "path": "/v1/embeddings",
    "method": "POST"
  }
}
```

**价格配置**：
- Model ratio: `0.05`
- Completion ratio: `0.05`（通常与 prompt 相同）

### 8.6 Rerank 模型示例

**端点配置**：
```json
{
  "jina-rerank": {
    "path": "/rerank",
    "method": "POST"
  }
}
```

### 8.7 多端点支持的模型示例

**同时支持 Chat、Responses、Embeddings 的模型**：
```json
{
  "openai": {
    "path": "/v1/chat/completions",
    "method": "POST"
  },
  "openai-response": {
    "path": "/v1/responses",
    "method": "POST"
  },
  "embeddings": {
    "path": "/v1/embeddings",
    "method": "POST"
  }
}
```

### 8.8 字符串格式端点配置（兼容格式）

```json
{
  "openai": "/v1/chat/completions",
  "anthropic": "/v1/messages"
}
```

### 8.9 规则匹配模型示例（前缀匹配）

**场景**：为所有 `gpt-4-*` 模型统一配置元数据

**配置**：
- Model Name: `gpt-4-`
- Name Rule: `Prefix Match`（前缀匹配）
- Description: `GPT-4 系列模型`
- Icon: `OpenAI`
- Tags: `["chat", "gpt-4"]`

**影响**：
- 所有以 `gpt-4-` 开头的模型（如 `gpt-4-turbo`、`gpt-4o`）都会展示此描述、图标、标签
- 但每个具体模型的价格仍由各自的 `ModelRatio` 配置决定

### 8.10 包含匹配模型示例

**场景**：为所有包含 `vision` 的模型统一配置

**配置**：
- Model Name: `vision`
- Name Rule: `Contains Match`（包含匹配）
- Tags: `["vision", "multimodal"]`

### 8.11 自定义端点路径示例

**场景**：将 Anthropic 模型的路径改为自定义路径

```json
{
  "anthropic": {
    "path": "/v2/messages",
    "method": "POST"
  }
}
```

**影响**：
- 模型广场展示该模型支持 `anthropic` 端点
- 前端 API 示例生成的 URL 为 `/v2/messages`
- 实际请求转发到上游的 `/v2/messages` 路径

### 8.12 禁用模型示例

**配置**：
- Status: `false`（关闭）

**影响**：
- 模型不展示在模型广场
- 用户无法调用（即使渠道中有该模型）

### 8.13 完整系统选项 JSON 示例

**ModelRatio**（按量计费倍率）：
```json
{
  "gpt-4": 15,
  "gpt-4o": 1.25,
  "gpt-4o-mini": 0.075,
  "gpt-3.5-turbo": 0.25,
  "claude-3-opus": 15,
  "claude-3-5-sonnet": 1.0,
  "gemini-1.5-pro": 1.25,
  "dall-e-3": 0
}
```

**ModelPrice**（按次计费价格）：
```json
{
  "dall-e-3": 0.04,
  "stable-diffusion-xl": 0.02,
  "midjourney": 0.1
}
```

**CompletionRatio**（完成 token 倍率）：
```json
{
  "gpt-4": 2,
  "gpt-4o": 4,
  "claude-3-opus": 5,
  "claude-3-5-sonnet": 5
}
```

**CacheRatio**（缓存命中倍率）：
```json
{
  "gpt-4o": 0.5,
  "claude-3-5-sonnet": 0.1
}
```

### 8.14 价格换算完整示例

**示例 1：GPT-4o**
- `ModelRatio = 1.25`
- `CompletionRatio = 4`
- 前端展示：
  - Input: `1.25 × 2 = $2.5 / 1M tokens`
  - Output: `2.5 × 4 = $10 / 1M tokens`
- 真实扣费（1000 input + 500 output tokens，group_ratio=1）：
  - `quota = (1000 + 500 × 4) × 1.25 × 1 = 7500 × 1.25 = 9375`
  - `USD = 9375 / 500000 = $0.01875`

**示例 2：Claude 3.5 Sonnet（带缓存）**
- `ModelRatio = 1.0`
- `CompletionRatio = 5`
- `CacheRatio = 0.1`
- 前端展示：
  - Input: `1.0 × 2 = $2 / 1M tokens`
  - Output: `2 × 5 = $10 / 1M tokens`
  - Cached: `2 × 0.1 = $0.2 / 1M tokens`
- 真实扣费（1000 input + 500 output + 200 cache tokens，group_ratio=1）：
  - `promptQuota = (1000 - 200) + 200 × 0.1 = 800 + 20 = 820`
  - `completionQuota = 500 × 5 = 2500`
  - `quota = (820 + 2500) × 1.0 × 1 = 3320`
  - `USD = 3320 / 500000 = $0.00664`

**示例 3：DALL-E 3（按次计费）**
- `ModelPrice = 0.04`
- 前端展示：`$0.04 / request`
- 真实扣费（group_ratio=1）：
  - `quota = 0.04 × 500000 × 1 = 20000`
  - `USD = 20000 / 500000 = $0.04`

---

## 附录：相关文件索引

### 前端文件

| 文件路径 | 作用 |
|---------|------|
| `web/default/src/routes/pricing/index.tsx` | 模型广场路由入口 |
| `web/default/src/routes/pricing/$modelId/index.tsx` | 模型详情页路由 |
| `web/default/src/features/pricing/index.tsx` | 模型广场主组件 |
| `web/default/src/features/pricing/api.ts` | 模型广场 API 调用 |
| `web/default/src/features/pricing/types.ts` | 类型定义 |
| `web/default/src/features/pricing/constants.ts` | 常量定义 |
| `web/default/src/features/pricing/hooks/use-pricing-data.ts` | 数据获取 Hook |
| `web/default/src/features/pricing/hooks/use-filters.ts` | 过滤器 Hook |
| `web/default/src/features/pricing/lib/filters.ts` | 过滤逻辑 |
| `web/default/src/features/pricing/lib/price.ts` | 价格计算 |
| `web/default/src/features/pricing/lib/dynamic-price.ts` | 动态定价 |
| `web/default/src/features/pricing/components/model-card.tsx` | 卡片视图 |
| `web/default/src/features/pricing/components/pricing-table.tsx` | 表格视图 |
| `web/default/src/features/pricing/components/pricing-columns.tsx` | 表格列定义 |
| `web/default/src/features/pricing/components/model-details.tsx` | 详情抽屉 |
| `web/default/src/features/pricing/components/model-details-api.tsx` | API 示例 |
| `web/default/src/routes/_authenticated/models/$section.tsx` | 控制台模型管理路由 |
| `web/default/src/features/models/index.tsx` | 控制台模型管理主页面 |
| `web/default/src/features/models/components/drawers/model-mutate-drawer.tsx` | **模型编辑抽屉表单** |
| `web/default/src/features/models/components/models-table.tsx` | 模型列表表格 |
| `web/default/src/features/models/constants.ts` | 常量（端点模板等） |
| `web/default/src/features/models/types.ts` | 类型定义 |
| `web/default/src/features/models/api.ts` | 模型 CRUD API |

### 后端文件

| 文件路径 | 作用 |
|---------|------|
| `model/model_meta.go` | Model 结构体定义与数据库操作 |
| `model/pricing.go` | Pricing 聚合结构与缓存逻辑 |
| `model/pricing_default.go` | 默认供应商映射 |
| `model/pricing_refresh.go` | 价格缓存刷新 |
| `model/model_extra.go` | 模型附加信息缓存 |
| `controller/pricing.go` | Pricing Controller |
| `controller/model_meta.go` | Model Meta Controller |
| `controller/ratio_sync.go` | 倍率同步 Controller |
| `dto/pricing.go` | Pricing DTO |
| `service/billing.go` | 计费服务 |
| `service/billing_session.go` | 计费会话 |
| `relay/helper/price.go` | 真实计费入口 |
| `setting/ratio_setting/model_ratio.go` | 倍率设置内存映射 |
| `common/endpoint_defaults.go` | 端点默认配置 |
| `common/endpoint_type.go` | 端点类型推断 |
| `constant/endpoint_type.go` | EndpointType 常量 |
| `pkg/billingexpr/` | 计费表达式（分层计费） |
| `router/api-router.go` | 路由注册 |

---

**文档版本**：v1.0
**最后更新**：2026-07-01
**项目**：new-api
