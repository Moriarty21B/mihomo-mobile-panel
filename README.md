# Mihomo Mobile Panel

面向小米路由器的手机 Mihomo 管理面板。用浏览器管理代理、设备、订阅和用量，支持添加到手机主屏幕。

界面采用轻量的 Liquid Glass 视觉效果：清晰的内容、克制的玻璃边缘和短动效，网页资源随程序提供，无需加载外部字体或前端 CDN。

## 功能

| 页面 | 功能 |
| --- | --- |
| 首页 | 全屋代理开关、当前节点、国内/国外连接检查、订阅剩余流量 |
| 节点 | 搜索、延迟排序、手动选择、台湾/新加坡/香港智能优选 |
| 服务分流 | 为 ChatGPT、TikTok、YouTube、X/Twitter、Netflix 单独选择节点 |
| 设备 | 按 MAC 保存策略，跟随规则/完全直连/强制 TCP 代理，支持排序、置顶与归档 |
| 用量 | 全屋和单台设备的代理用量、每日曲线、实时代理网速 |
| 订阅与规则 | 多订阅切换、机场套餐余额、本地规则方案、更新订阅后自动沿用规则 |
| 实时连接 | 网站、设备、实际节点、命中规则、速度和用量，可断开单条连接 |
| 维护 | 日志轮转、规则修复、配置校验、有限次数自动恢复、备份与恢复 |

内置 220 条常用规则：Steam、常见游戏分发、系统更新、国内服务及 **Xbox Live/Xbox 直连**；国外服务按规则选择节点。可另存自己的规则方案。

## 兼容环境

这是针对小米原厂固件的适配，当前网络逻辑使用下表中的固定 LAN 网段和端口。`--host` 仅改变安装器的 SSH 连接地址，不代表已支持其他 LAN 网段。

| 项目 | 默认值 |
| --- | --- |
| 系统/架构 | 小米路由器 Linux ARM64，iptables NAT；开发适配机型 BE6500 Pro |
| LAN | `192.168.31.0/24`，路由器 `192.168.31.1`，接口 `br-lan` |
| Mihomo 目录 | `/data/other/mihomo` |
| mixed / redir / tproxy | `7890` / `7892` / `7893` |
| DNS | `192.168.31.1:1053`，TCP/UDP |
| Mihomo API | `127.0.0.1:9091`，自动读取现有配置中的 API secret |
| 网页面板 | `http://192.168.31.1:9080/` |
| 节点组 | `节点选择`、`自动选择` |

需要事先有正常运行的 Mihomo、配置和规则数据库，并已具备 root SSH 访问。项目不附带代理节点、机场订阅、Mihomo 内核或规则数据库。它管理现有安装。

透明代理只接管 IPv4 TCP；DNS TCP/UDP 53 按设备策略处理。普通 UDP 业务保持直连。IPv6 流量不纳入这套接管逻辑。

## 安装

从 [Releases](https://github.com/Moriarty21B/mihomo-mobile-panel/releases) 获取安装包，或按下一节自行构建。

1. 电脑连接路由器 LAN，把整个安装包解压到同一个文件夹。
2. 双击 `安装或更新.bat`，输入自己的 root 密码。
3. 首次连接会显示路由器 SSH 指纹，核实后输入 `yes`；以后指纹变化时会停止连接。
4. 手机连接路由器 Wi-Fi，访问 `http://192.168.31.1:9080/`。首次安装的面板登录密码与本次输入的路由器密码相同。

安装器按 SSH 地址、端口保存 Windows DPAPI 保护的密码和已确认指纹，存放于当前用户的 `MihomoMobilePanelOpenSource` 本地应用数据目录。需要重新输入密码时运行 `重新输入密码.bat`。

安装器参数：

```powershell
.\mihomo-mobile-panel-installer.exe --host 192.168.31.1 --ssh-port 22
.\mihomo-mobile-panel-installer.exe --fingerprint SHA256:填写自己核实的指纹
.\mihomo-mobile-panel-installer.exe --install-boot
```

开机恢复默认不安装。`--install-boot` 仅在已验证的持久 crontab 环境中启用面板/代理恢复，保留已有厂商任务；默认不重新开启 SSH，也不保存 SSH 私钥。后端另有显式的 `-boot-ssh` 高级选项，只有与 `-install-boot` 一起使用才启用 SSH 恢复。

此公开版从原有私人面板整理而来。公开版本的安装器和合成测试在本地验证；首次用于其他固件时，需要自行核实接口、挂载点、命令与端口兼容性。

## 构建

需要 Go 1.27.1 或更新版本、Windows PowerShell，以及首次下载 Go 依赖所需的网络。

```powershell
git clone https://github.com/Moriarty21B/mihomo-mobile-panel.git
cd mihomo-mobile-panel
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

构建脚本执行测试和静态检查，然后生成 ARM64 面板和 Windows 安装器，输出到 `dist/`。通过固定版本的 `github.com/akavel/rsrc` 从源码嵌入 `asInvoker` 清单，安装器无需 Windows 管理员权限。安装器只嵌入本次构建的面板，不依赖历史私人二进制。发布构建使用 `-trimpath`，不嵌入开发电脑的目录。

后端单独构建：

```powershell
cd mobile
go test ./...
go vet ./...
$env:GOOS='linux'; $env:GOARCH='arm64'; $env:CGO_ENABLED='0'
go build -trimpath -buildvcs=false -ldflags='-s -w' -o ../mihomo-mobile .
```

所有测试使用合成设备与配置，不需要私人备份、订阅或真实路由器。

## 数据口径

- 设备用量只计实际经过代理节点的上传和下载；`DIRECT` 流量不计入代理用量。
- 连接与网速按约 5 秒采样，可能漏掉很短的连接；机场计费以提供方账单为准。
- 首页套餐余额来自订阅返回的信息，未提供时显示不可用，不用本地估算伪造余额。
- 完全直连的设备绕过 Mihomo，连接页无法观察其完整活动；“未观察到”不代表没有联网。
- 平台检查验证公开页面的连通性。验证页会标记为无法确认，不能据此保证账号登录、GPT 对话或流媒体播放。

## 配置与隐私

运行数据位于 `/data/other/mihomo/mobile-panel/`，包含订阅、设备策略、统计和备份。它们属于本机数据，不应提交到 GitHub。详见 [PRIVACY.md](PRIVACY.md)。

本仓库使用全新的公开提交记录，仅含源码、合成测试、公共规则和文档。请勿把私人安装目录、旧 ZIP、截图或完整备份复制进仓库。

面板用于可信 LAN 管理。登录凭据及 API token 通过本地 HTTP 使用，不要直接映射到公网；需要远程访问时使用自己的 VPN 或受控 HTTPS 入口。

## 许可与致谢

项目代码使用 [MIT License](LICENSE)。第三方组件保留各自许可，见 [THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt) 与 `third_party/licenses/`。

玻璃折射实现改编自 [rizzytoday/liquid-glass](https://github.com/rizzytoday/liquid-glass)，保留其 MIT 许可。Liquid Glass 是视觉参考，本项目与 Apple、小米及 Mihomo 官方没有隶属关系。
