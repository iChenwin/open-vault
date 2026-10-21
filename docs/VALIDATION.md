# 验证入口

当前分支为 CBT-only，已移除旧文件备份引擎及其集成测试。历史版本的通过结果不能证明当前 CBT 内核能力。

```sh
go test ./...
go vet ./...
```

Linux CI 运行 race detector、加密镜像恢复测试、用户态驱动协议测试、CLI 协调和真实 Unix socket/子进程集成，并构建三个程序。Windows 的 Unix socket/内核相关用例会明确跳过。

真实内核实验必须使用 [CBT-LAB.md](CBT-LAB.md)，结果及失败分析记录在 [CBT-VALIDATION.md](CBT-VALIDATION.md)。以测试数据展示的浏览器截图只证明界面运行。
