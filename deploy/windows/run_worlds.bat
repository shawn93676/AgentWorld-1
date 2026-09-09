@echo off
rem =====================================================================
rem  AgentWorld - Village + Economy 双世界（跨世界旅行演示）
rem  同一台 Windows 服务器跑两个独立进程，通过 HTTP /life 端点互联。
rem
rem  用法：
rem    1. 先编译两个 exe（在项目根目录执行）：
rem         go build -o bin/economy.exe ./worlds/economy/cmd/economy
rem         go build -o bin/village.exe  ./worlds/village/cmd/village
rem    2. 把本文件放到项目根目录（与 bin\ 同级），双击运行。
rem    3. 浏览器打开 http://localhost:19200
rem         事件流: http://localhost:19200/api/events
rem
rem  说明：默认全部用 localhost（同一台机器）。要分开部署到不同机器时，
rem        把下面 ECO_LIFE_URL / VILLAGE_LIFE_URL 改成对方机器可达的地址即可，
rem        代码无需任何改动。
rem =====================================================================

rem ---- LLM（两个世界共用；留空则走离线 Mock，零 token）----
set LLM_API_KEY=sk-your-deepseek-api-key-here
set LLM_BASE_URL=https://api.deepseek.com/v1
set LLM_MODEL=deepseek-chat

rem ---- Economy 世界（监听 :19301 作为 /life 目的地，:19100 为观测台）----
set ECO_LIFE_ADDR=:19301
set ECO_LIFE_URL=http://localhost:19301/life
set ECO_DB=economy.db
set ECO_OBS_ADDR=:19100
set ECO_INTERVAL=5s
set ECO_TICK=8s

rem ---- Village 世界（Web 与 /life 共用 :19200）----
set VILLAGE_ADDR=:19200
set VILLAGE_LIFE_URL=http://localhost:19200/life
set VILLAGE_DB=village.db
set VILLAGE_SNAPSHOTS=village_worlds
rem 演示模式：启用后铁匠 Marcus 会在本地机会不足时自主前往 Economy 谋生，
rem 打工后带技艺返回。删掉下面这行则不会自动出发（仍可在代码/API 手动触发）。
set VILLAGE_DEMO_UID=marcus

rem ---- 启动（两个进程各自独立窗口并行运行）----
echo ============================================================
echo  AgentWorld 双世界启动中...
echo    Economy :  life %ECO_LIFE_URL%   obs :19100
echo    Village :  web+life %VILLAGE_LIFE_URL%
echo  打开浏览器:  http://localhost:19200
echo ============================================================
start "AgentWorld-Economy" bin\economy.exe
start "AgentWorld-Village" bin\village.exe

echo.
echo 两个世界已在独立窗口启动。关闭本窗口不会停止它们。
echo 想停止时，直接关掉 "AgentWorld-Economy" 与 "AgentWorld-Village" 两个窗口（或各自 Ctrl+C）。
echo 按任意键关闭本启动窗口...
pause >nul
