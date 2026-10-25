# FastResearch 模块启动桥

基于官方 FastResearch `main` 分支 `6c0026` 制作；补丁尚未推送或部署到门户服务器。FastTask 的模块入口使用以下固定地址，不携带个人 Key、票据或任意 `next`：

```text
https://fastrd.io/workspace?launch=fast-write
https://fastrd.io/workspace?launch=fast-read
https://fastrd.io/workspace?launch=fast-news
https://fastrd.io/workspace?launch=fast-ppt
```

门户复用已保存的成员会话并向 `/api/content/me` 校验。过期时打开现有个人 Key 登录框，登录完成后恢复启动；待办存储只包含模块 ID。FastWrite 调用原有 `/api/fastwrite/login-url`；FastRead、FastNews 使用固定 catalog 地址构造 `/api/sso/launch`；FastPPT 跳转至其 catalog URL。全部在当前标签页跳转，不在 effect 中弹窗或关闭窗口。现有 `launch=fast-task` 回调保持原样。

在 FastResearch 仓库应用并验证：

```powershell
git apply --check C:\path\to\module-launch.patch
git apply C:\path\to\module-launch.patch
npm ci --ignore-scripts
node --test src/module-launch.test.mjs
npm run build
```

部署沿用门户已有的模块地址配置：`VITE_READ_URL`、`VITE_FASTNEWS_URL`、`VITE_FASTPPT_URL` 及服务器的 FastWrite 配置。生产环境需设置实际服务地址；补丁不把开发默认地址替换为猜测的公网域名。

已验证 TypeScript 与生产构建、7 项隔离单测，以及浏览器 StrictMode 下四模块单次跳转、过期会话登录恢复、原始错误展示。浏览器验证使用模拟服务，未访问真实成员数据或签发真实票据。FastTask 部署完成后，门户也需要应用此补丁并重新构建部署，入口才能自动完成模块跳转。
