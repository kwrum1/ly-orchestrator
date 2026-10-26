# 03 · 设计系统

> 设计语言：**浅色、克制、清晰、可运维**。
> 参考 iKuai 4.0 的浅蓝白层级、UniFi 的细线与数据密度、Panabit 的表格/会话可读性；
> 不使用任何第三方商标、图标、截图或专有 CSS。

## 1. 设计原则

1. **先回答“网络现在怎么样”**：状态、异常、待处理永远在首屏可见。
2. **数据密度优先，但不拥挤**：表格 44px 行高、13px 正文、数字等宽；卡片留白 20px。
3. **只用真实数据**：未知 `--`，不可用显示原因；0 是 0，不是“无数据”。
4. **操作可逆**：保存只落 desired；应用是显式事务；删除有依赖影响提示。
5. **颜色是辅助**：状态同时有文字/图标；不以颜色作为唯一信息。
6. **一致性**：同一类对象（接口/线路/策略/规则）在首页、列表、详情中使用同一套字段名与单位。

## 2. 色彩 Tokens

### 2.1 基础色

| Token | 值 | 用途 |
| --- | --- | --- |
| `--bg-canvas` | `#f4f7fb` | 页面背景 |
| `--bg-subtle` | `#eef3f9` | 次级背景、表头 |
| `--surface` | `#ffffff` | 卡片、表格、弹窗 |
| `--surface-sunken` | `#f7f9fc` | 卡片内分区、代码块 |
| `--sidebar` | `#fbfcfe` | 侧栏 |
| `--text-primary` | `#172033` | 标题、关键值 |
| `--text-secondary` | `#5c6b80` | 正文、说明 |
| `--text-tertiary` | `#8a97a8` | 占位、时间戳 |
| `--text-inverse` | `#ffffff` | 主按钮文字 |
| `--border` | `#e4eaf2` | 卡片/表格分割线 |
| `--border-strong` | `#d3dce8` | 输入框、悬停边框 |
| `--accent` | `#2563eb` | 主操作、链接、选中 |
| `--accent-hover` | `#1d4ed8` | 主按钮 hover |
| `--accent-soft` | `#eaf2ff` | 选中菜单、浅色标签 |
| `--accent-soft-2` | `#dbeafe` | 进度/高亮底 |
| `--success` | `#0f9f6e` | 正常、在线、已应用 |
| `--success-soft` | `#e8f8f1` | 成功标签底 |
| `--warning` | `#d97706` | 降级、待应用、注意 |
| `--warning-soft` | `#fff6e5` | 警告标签底 |
| `--danger` | `#dc2626` | 失败、删除、不可用 |
| `--danger-soft` | `#fdecec` | 错误标签底 |
| `--info` | `#0ea5e9` | 信息、代理/外部 |
| `--info-soft` | `#e8f6fd` | 信息标签底 |
| `--violet` | `#7c5cfc` | 终端/用户类强调 |
| `--violet-soft` | `#f1edff` | 紫色 KPI 底 |

### 2.2 KPI 渐变底（仅首页/概览）

```css
--kpi-blue:   linear-gradient(135deg, #eef5ff 0%, #f8fbff 100%);
--kpi-violet: linear-gradient(135deg, #f4f0ff 0%, #fbf9ff 100%);
--kpi-amber:  linear-gradient(135deg, #fff7e8 0%, #fffdf8 100%);
--kpi-green:  linear-gradient(135deg, #ecfbf5 0%, #f8fffc 100%);
```

### 2.3 图表色

固定顺序，避免隐藏系列后重排：

```text
#2563eb  #0ea5e9  #10b981  #f59e0b  #8b5cf6  #ef4444  #14b8a6  #64748b
```

上行/下行建议：

- 上行：`#8b5cf6`（紫）
- 下行：`#2563eb`（蓝）
- 总和：`#0f9f6e`（绿）
- 未知/过期：`#94a3b8`（灰）

### 2.4 状态映射

| 语义 | 颜色 | 文案示例 |
| --- | --- | --- |
| running / available / applied / online | success | 运行中 / 可用 / 已应用 / 在线 |
| desired_not_applied / pending / stale | warning | 待应用 / 读取中 / 数据过期 |
| degraded / partial | warning + 图标 | 降级 / 部分可用 |
| unavailable / failed / offline / denied | danger | 不可用 / 失败 / 离线 / 无权限 |
| unknown / not_configured | neutral | 未知 / 未配置 |

> 状态组件必须同时有文字；Badge 颜色不能单独承载语义。

## 3. 字体与排版

```css
--font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
             "Hiragino Sans GB", "Microsoft YaHei", "Noto Sans SC", sans-serif;
--font-mono: "SFMono-Regular", Consolas, "Liberation Mono", monospace;
```

| 层级 | 字号/行高 | 字重 | 用途 |
| --- | --- | --- | --- |
| Display | 28/36 | 600 | 首页大数字（可选） |
| H1 | 20/28 | 600 | 页面标题 |
| H2 | 16/24 | 600 | 卡片标题 |
| H3 | 14/22 | 600 | 分区标题 |
| Body | 13/20 | 400 | 表格/正文 |
| BodyStrong | 13/20 | 600 | 关键值 |
| Caption | 12/18 | 400 | 说明、时间 |
| Micro | 11/16 | 500 | 标签、单位 |

规则：

- 数字使用 `font-variant-numeric: tabular-nums;`；速率/字节/端口右对齐。
- 中文正文不要小于 12px；表格主字段 13px。
- 标题不使用全大写英文；单位与数值之间留 4px。
- 页面标题最多一行，超出省略并提供 title。

## 4. 间距 / 圆角 / 阴影

| Token | 值 |
| --- | --- |
| `--space-1` | 4px |
| `--space-2` | 8px |
| `--space-3` | 12px |
| `--space-4` | 16px |
| `--space-5` | 20px |
| `--space-6` | 24px |
| `--space-8` | 32px |
| `--space-10` | 40px |
| `--space-12` | 48px |
| `--radius-sm` | 4px |
| `--radius-md` | 6px |
| `--radius-lg` | 8px |
| `--radius-xl` | 10px |
| `--radius-2xl` | 12px |
| `--shadow-card` | `0 1px 2px rgba(16,24,40,.04)` |
| `--shadow-pop` | `0 8px 24px rgba(16,24,40,.10)` |
| `--shadow-drawer` | `-12px 0 32px rgba(16,24,40,.12)` |

卡片默认：`background: var(--surface); border: 1px solid var(--border); border-radius: 10px;`
不使用大面积阴影；阴影只用于浮层/抽屉。

## 5. 布局与响应式

### 5.1 断点

| 名称 | 宽度 | 侧栏 | 页面 padding | 卡片列 |
| --- | --- | --- | --- | --- |
| Wide | ≥ 1800 | 240px | 32px | 12 栅格 |
| Standard | 1440–1799 | 224px | 24px | 12 栅格 |
| Compact | 1200–1439 | 208px / 折叠 64px | 20px | 12 栅格，低优先列隐藏 |
| Tablet | 900–1199 | 抽屉式 | 16px | 1–2 列 |
| Mobile | < 900 | 抽屉式 | 12px | 1 列 |

内容最大宽度 `1600px`，居中；超宽屏不把表格拉到不可读。

### 5.2 结构尺寸

| 元素 | 1440 | 1920 |
| --- | --- | --- |
| 顶栏高度 | 56px | 56px |
| 侧栏宽度 | 224px | 240px |
| 页面标题区 | 64px | 72px |
| 卡片 padding | 20px | 20–24px |
| 卡片标题行 | 44px | 48px |
| KPI 卡高度 | 104px | 112px |
| 首页主图高度 | 300px | 340px |
| 表格行高 | 44px | 44px |
| 表头高度 | 40px | 40px |
| 弹窗最大高度 | 82vh | 82vh |
| 抽屉宽度 | 520px | 600px |

### 5.3 12 栅格

- 1440：内容宽约 1168px（1440−224−2×24），12 列 + 16px gap。
- 1920：内容宽约 1616px（1920−240−2×32），最大 1600px，12 列 + 20px gap。
- 首页 KPI：5 卡等分；标准屏每卡约 218px，宽屏约 300px。
- 首页主区：标准 8+4；宽屏 9+3；平板 12+12；手机 12。

## 6. 组件规范

### 6.1 按钮

| 类型 | 样式 | 用途 |
| --- | --- | --- |
| Primary | 蓝底白字，`--radius-md`，高 34/36px | 保存并应用、确认 |
| Secondary | 白底、1px border、深色字 | 取消、次要操作 |
| Ghost | 无边框，hover 浅灰 | 表格行内、图标按钮 |
| Danger | 红字/红底（确认时） | 删除、初始化 |
| Link | 蓝色文字 | 详情、跳转 |

- 高度：默认 34px；表格内 28px；移动端 40px。
- 图标 16px，图标与文字间距 6px。
- 禁用：`opacity: .45`，保留 cursor 说明原因（title）。
- 加载：按钮内 spinner，禁止重复提交；不要全屏遮罩阻断阅读。

### 6.2 输入 / 选择

- 高度 34px；圆角 6px；边框 `--border-strong`；focus 2px `--accent-soft-2` + 1px accent。
- 标签在上方，12px secondary；必填用 `*`，不要只靠红色。
- 错误：边框 danger + 12px 错误文案；错误定位到字段。
- 只读：`surface-sunken` 背景；不伪装成可编辑。
- 密码：右侧眼睛按钮；编辑时留空表示不变，placeholder “已保存（不显示）”。

### 6.3 Tabs

- 下划线式；选中 2px accent；未选中 secondary；高度 40px。
- Tab 数量 ≤ 5；超过用下拉。
- 切换 Tab 不改变页面滚动位置（除非内容差异大）。

### 6.4 卡片

```text
┌─────────────────────────────────────┐
│ 标题                    [操作/筛选] │ 48px
│ 说明（可选）                         │
├─────────────────────────────────────┤
│ 内容                                 │
└─────────────────────────────────────┘
```

- 卡片之间 16/20px；卡片内分区用 1px 线，不用嵌套阴影。
- 卡片可点击时 hover 边框变 accent-soft；不要整卡变蓝。

### 6.5 Badge / 状态点

- 高度 20/22px；圆角 999；12px；底色 soft，文字深色。
- 状态点 6px + 文字；offline 灰，degraded 黄，failed 红。
- 长原因不放 Badge；用 Tooltip 或详情抽屉。

### 6.6 表格（核心）

| 属性 | 默认 | 紧凑 | 舒适 |
| --- | --- | --- | --- |
| 行高 | 44px | 38px | 52px |
| 单元格左右 padding | 12px | 8px | 16px |
| 表头高 | 40px | 36px | 44px |
| 字号 | 13px | 12px | 13px |
| 分割线 | 1px `--border` | 同 | 同 |

规则：

- 表头：`--surface-sunken` 背景、secondary 文字、12px、字重 500。
- 首列可放复选框/图标；操作列右对齐、固定。
- 空表：只显示一个居中的空态块（图标 + 标题 + 说明 + 主操作），不渲染 8 行空白。
- 加载：表头保留，body 显示 5 行 skeleton；不要整页 spinner。
- 行 hover：`--accent-soft` 的 40% 透明度；选中行用更明显底色 + 左侧 2px accent。
- 横向滚动：表头与首列/操作列 sticky；滚动容器有渐变阴影。
- 列设置：gear 图标打开 checkbox 列表；至少保留 3 列。
- 排序：点击表头切换 asc/desc/none；排序列显示箭头。
- 复制：IP/MAC/端口可点击复制，toast “已复制”。
- 批量：只有后端支持批量语义才显示；否则不显示“批量操作”。

### 6.7 弹窗 / 抽屉

| 类型 | 宽度 | 用途 |
| --- | --- | --- |
| Confirm | 420px | 删除/初始化/角色变更 |
| Small form | 560px | 用户、简单对象 |
| Standard form | 680px | LAN/WAN、DHCP、DNS、端口映射 |
| Large form | 840px | WAN 群组、接口聚合 |
| Policy form | 960px（max 92vw） | 策略路由/流量控制 |
| Drawer | 520/600px | 在线用户会话、详情 |
| Full drawer | 720px | 运行态预览（大 JSON/表格） |

- 弹窗：标题 16px、内容可滚动、底部操作固定；ESC/遮罩关闭需确认脏表单。
- 抽屉：从右滑入 180ms；标题区 64px、底部操作 64px；移动端全屏。
- 焦点陷阱、打开时聚焦第一个输入、关闭后回到触发按钮。
- 不使用嵌套弹窗；复杂表单用抽屉/分步。

### 6.8 Toast / Banner

- Toast：右上角，3s 自动消失；成功/失败/信息；失败可展开 `request_id`。
- Banner：页面顶部，用于“待应用/降级/升级中/只读”；可关闭的仅信息类，安全类不可关闭。
- Apply Banner 固定显示事务号、已用时长、状态；不遮挡操作。

### 6.9 空态 / 错误态 / 加载态

| 状态 | 表现 |
| --- | --- |
| 首次加载 | 卡片 skeleton；表格 5 行 skeleton；不显示 0 |
| 刷新中 | 顶部细进度条或按钮 spinner；保留旧数据，标“更新中” |
| 空数据 | 图标 + “暂无 X” + 说明 + 主操作（若可创建） |
| 不可用 | 灰色卡片 + reason + “重试”；不显示 0 作为真实值 |
| 权限不足 | 403 页/区块，说明只读角色；不显示空表 |
| 请求失败 | 错误块 + HTTP/code + request_id + 重试 |
| 慢请求 | 骨架 + “状态读取较慢，仍在等待…”；>20s 显示可重试 |

## 7. 图标与品牌

- 图标：Lucide，线性，1.75px stroke，16/18/20px；不混用填充图标。
- 产品标识：保留现有 “LR / Ly Route” 文字标识或自有几何图形；不使用 iKuai/UniFi/Panabit 的 logo。
- 登录页不使用第三方产品截图/背景；用浅色渐变 + 细网格/几何线，保持中性。
- 禁用词检查：`iKuai`、`UniFi`、`Ubiquiti`、`Panabit` 不得出现在生产 UI 资源中（文档除外）。

## 8. 动效

- 时长：hover 120ms；弹窗/抽屉 180ms；图表 200ms（仅首次）。
- 缓动：`cubic-bezier(.2,.8,.2,1)`。
- 尊重 `prefers-reduced-motion`：关闭滑入/图表动画。
- 轮询刷新不闪烁、不重新播放入场动画。

## 9. 文案与单位

- 中文为主；保留后端术语原文（如 `desired_not_applied`）时可放在详情/技术信息中，
  主界面用用户语言（“待应用”）。
- 速率：`b/s`、`Kbps`、`Mbps`、`Gbps`；字节：`B/KiB/MiB/GiB/TiB`（1024 进制，与后端一致）。
- 时间：`YYYY-MM-DD HH:mm:ss`；相对时间用于列表（“3 分钟前”），悬停显示绝对时间。
- 端口：`1–65535`；范围用 `-`；协议大写 `TCP/UDP/ICMP`。
- 未知：`--`；不可用：`不可用` + reason；加载：`读取中`。
- 不要写“智能诊断”“一键优化”等没有后端支撑的文案。

## 10. 深浅色

v1 只做浅色（与 iKuai 4.0 参考一致，降低验收面）。Tokens 已按语义命名，后续可加
`[data-theme="dark"]` 覆盖：

- `--bg-canvas: #0f141b`
- `--surface: #171e27`
- `--border: #263140`
- `--text-primary: #e6edf6`
- `--accent: #4c8dff`

深色不在本轮验收范围；如产品要求，单独排期并补对比度/图表色测试。

## 11. 设计验收硬指标

- 1440×900 与 1920×1080 无横向页面滚动（表格内部滚动除外）。
- 表格行高、字号、间距与本文一致；空表不出现空白占位行。
- 所有状态有文字，不仅颜色；对比度 ≥ 4.5:1（正文）。
- 弹窗/抽屉尺寸符合 §6.7；ESC/焦点/滚动锁行为正确。
- 首页首屏在 1440 至少显示：健康状态、4 个 KPI、WAN 趋势、WAN 出口/接口状态。
- 不使用第三方商标/素材；生产资源扫描通过。
