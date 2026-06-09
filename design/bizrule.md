# 业务标识提取规则（bizrule）

## 背景

不同磁盘/卷有各自的文件命名和目录组织规则。业务标识（物料号+版本）的提取逻辑因盘而异，需要做成可插拔的代码块。

## 接口定义

```go
// VolumeRule 针对单个卷的业务规则
type VolumeRule interface {
    // ExtractBizKey 从文件完整路径提取业务标识
    // 无法识别时返回空字符串
    ExtractBizKey(path string) string
}
```

## 注册方式

在 client 配置中，按 server_id + volume 注册规则实现：

```go
rules := bizrule.Registry{
    {"server-A", "D:"}: &DrawingsRule{Root: `D:\drawings\`},
    {"server-A", "E:"}: &DocsRule{Root: `E:\docs\`},
    {"server-B", "D:"}: &DrawingsRule{Root: `D:\drawings\`},
}
```

部署新磁盘：新增对应 Rule 实现 → 在此注册 → 其余代码不变。

## 业务标识格式

`{物料号}_{版本}` 例如：`1234-56_A`、`09AB-CD_REV2`

由各 Rule 实现自行定义如何从路径中解析，没有全局约定。

## 示例实现

```go
// DrawingsRule：图纸盘，物料号+版本来自倒数第二层目录名
// 路径示例：D:\drawings\1234-56_A\file.pdf → biz_key = "1234-56_A"
type DrawingsRule struct {
    Root string
}

func (r *DrawingsRule) ExtractBizKey(path string) string {
    rel := strings.TrimPrefix(path, r.Root)
    parts := strings.Split(rel, `\`)
    if len(parts) < 2 {
        return ""
    }
    return parts[0] // 第一层目录即为物料+版本
}
```

## 说明

- 提取失败（路径不符合规则）返回空字符串，`biz_key` 写 NULL，不影响监听流程
- 规则代码在 `internal/bizrule/` 下，每个文件对应一类规则
- 规则只负责提取，不做任何 DB 操作
