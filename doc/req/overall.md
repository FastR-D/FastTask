# 0\. FastResearch开发规划

## FastResearch Tools

目标：8月上线。

- 找论文工具：FastNews，快速找论文，自动追踪感兴趣的领域的论文以及新闻。根据关键词订阅，以及根据主题完成报告。

- 读论文工具：FastRead，知识库，帮助快速读论文，深度思考，推导和记录idea。对标NotebookLM。

- 写论文工具：FastWrite，agent自动初稿，逐段精修。GPT生成对抗审稿意见，解决审稿意见。对标Primsm。

- 灵感捕捉工具：FastInsight，类似于OpenClaw，在飞书群的Bot，平时看到好的论文、新闻，截图发给Bot，Bot帮助找到论文，发给对应方向的同学

- 时间管理工具：FastTask/3Singals，自动筛选出每天最重要的事，帮忙制定合理的计划以及Milestone，推送给每个人

- PPT工具：FastPPT，在之前CodeX SKILL基础上，做我们自己的工具

- 自动Agent编排工具：FastLabs，自动调用Codex/Claude\-Code等，去并行跑任务（Parallel Goals），基于Map\-Reduce（或者DAG）分解任务，将任务分发给多个Agent并行跑，例如：开10个Instance跑20组实验，每个Agent跑2组实验。



代码：https://github\.com/orgs/FastR\-D/repositories

|工具|负责人|参与人员|进度|
|---|---|---|---|
|[1\. FastNews](https://aaum0ovr97g.feishu.cn/docx/U6U5dFzkeoCdaHxDv8UcPV8Ln8c?from=from_copylink)|技术：夏桐<br>产品：王佳瑶，吴昊，何艺||能够收集顶会文章并在前端进行展示基本信息和网址|
|[2\. FastRead](https://aaum0ovr97g.feishu.cn/docx/PfNVdl5cLoL6HDxtHrPcRBWOnFy?from=from_copylink)<br>|技术：秦希、刘浩宇<br>产品：王佳瑶<br>||1\.能够导入论文并进行速读<br>2\.能基于知识库进行回答，生成相应知识图谱<br>3\.能记录Idea<br>4\.可以生成文章综述和研究进展<br>5\.能检测论文间的结论矛盾和对论文进行漏洞挖掘|
|[3\. FastWrite](https://aaum0ovr97g.feishu.cn/docx/IkoCduWoOocpqPxrdEoc0eD5nId?from=from_copylink)<br>|技术：何艺、霍从儒、刘浩宇<br>产品：王佳瑶，何艺|||
|[4\. FastInsight](https://aaum0ovr97g.feishu.cn/docx/C6A4dTaQUoQlv6xCaMvcmg94nYf)<br>|技术：夏桐<br>产品：吴昊，何艺<br>||1\.在飞书群组中添加bot<br>2\.发送截图给bot识别其中论文基本信息<br>3\.总结论文内容并给出对应网址，分发给对应方向同学|
|[5\. FastTask](https://aaum0ovr97g.feishu.cn/docx/MIV9dqabaoL9dmxvbD1c93BfnZb)|技术：李旭祺、曾浩正、吴昊<br>产品：吴昊，何艺|||
|[6\. FastPPT](https://aaum0ovr97g.feishu.cn/docx/SQ32d5dyHo7QpgxHMUTcsS0hnmh)|技术：吴昊、霍从儒<br>产品：吴昊，何艺|||
|[7\. FastLabs](https://aaum0ovr97g.feishu.cn/docx/DkFKdR08Eol3wYxCB4Jcj5z3nJb?from=from_copylink)|技术：肖修齐<br>产品：何艺|||

说明：

1. 所有功能优先以满足我们自己场景的需求为主，提升我们自己的效率。最终打造成开源产品。

2. 底层Agent框架优先基于Clode\-Code/Codex SDK开发，我们只做上层通用Harness，Agent和模型可切换。

> 
> 
> 

3. 最终是基于实验室Mini主机部署，~~Cloudflare ~~~~Tunnel~~~~来公网访问。~~目前为了开发方便，可以直接阿里云部署。

> 阿里云主机：
> 
> 

4. 先基于已经备案的域名（tsinbei\.cn?0x535a\.cn?）直接部署，通过阿里云代理到内部的Mini主机。





## FastResearch Pannel

> 开发一个简单的控制面板，基于Token/账号（登陆尽可能简单），作为一个入口界面，其他工具都可以在这个界面点击进入。
> 
> 显示要读的论文，以及任务。
> 
> 

https://github\.com/FastR\-D/FastResearch

