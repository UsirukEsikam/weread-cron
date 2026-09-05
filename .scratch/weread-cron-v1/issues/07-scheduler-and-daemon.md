# 07: 调度与 daemon

**What to build:** 每日调度闭环：`nextStart(now, window, lastTerminal, tz)` 纯决策函数（Run Window 只约束开始时间、终态门控、`[now, 窗口结束]` 重排、错过不补跑、`start==end` = 固定时刻）+ daemon 循环（恢复 Login Session → 计算下次启动 → 睡眠到点 → 启动前再校验当天终态 → 执行 Task → 排次日）。TZ 由内嵌 tzdata + 环境变量决定（ADR-0003）。daemon 启动失败路径经薄 CLI seam 验证。

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] `nextStart` 决策正确：窗口只约束开始、终态（success/failed）存在 → 明天、无终态且窗口未过 → `[now, 结束]` 内随机、窗口已过 → 明天、`start==end` 固定时刻、跨午夜窗口为配置错误
- [ ] daemon 在注入 clock 下到点执行 Task，完成后写终态并排定次日
- [ ] 当天已有终态（seed 文件）→ daemon 不自动执行
- [ ] 启动时当天窗口未过且无终态 → 当天剩余窗口内随机安排一次
- [ ] 错过的整天不补跑
- [ ] 缺 Cookie / 非法配置下 daemon 启动失败（薄 CLI seam：exit code + stderr）
- [ ] TZ 生效：默认 Asia/Shanghai，`TZ` 可覆盖