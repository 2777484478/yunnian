import React, { useMemo, useState } from 'react';
import { Alert, Image, Space, Table, Tag, Typography } from 'antd';
import { PageHeader } from '@/components/ui';

import imgLogin from '@/assets/docs/01-login.webp';
import imgOverview from '@/assets/docs/02-overview.webp';
import imgProjects from '@/assets/docs/03-projects.webp';
import imgBuilds from '@/assets/docs/04-builds.webp';
import imgImages from '@/assets/docs/05-images.webp';
import imgTopology from '@/assets/docs/06-topology.webp';
import imgEnvDetail from '@/assets/docs/07-env-detail.webp';
import imgDeploys from '@/assets/docs/08-deploys.webp';
import imgRollbacks from '@/assets/docs/09-rollbacks.webp';
import imgAi from '@/assets/docs/10-ai.webp';
import imgProjectDetail from '@/assets/docs/11-project-detail.webp';
import imgEnvServices from '@/assets/docs/12-env-services.webp';
import imgBatchDeploy from '@/assets/docs/13-batch-deploy.webp';

const { Title, Paragraph, Text } = Typography;

// --------------------------------------------------------------------------- 排版积木

const SECTIONS: { id: string; title: string }[] = [
  { id: 'start', title: '1. 快速开始' },
  { id: 'ci', title: '2. CI 镜像构建' },
  { id: 'cd', title: '3. CD 环境部署' },
  { id: 'batch', title: '4. 批量发布' },
  { id: 'ai', title: '5. AI 运维助手' },
  { id: 'status', title: '6. 状态含义对照' },
  { id: 'faq', title: '7. 常见问题' },
];

/** 章节容器。 */
const Section: React.FC<{ id: string; title: string; desc?: React.ReactNode; children: React.ReactNode }> = ({
  id,
  title,
  desc,
  children,
}) => (
  <section id={`doc-${id}`} style={{ scrollMarginTop: 16, marginBottom: 40 }}>
    <Title level={4} style={{ marginTop: 0, marginBottom: desc ? 4 : 14 }}>
      {title}
    </Title>
    {desc ? (
      <Paragraph type="secondary" style={{ marginBottom: 14 }}>
        {desc}
      </Paragraph>
    ) : null}
    {children}
  </section>
);

/** 小节标题。 */
const Sub: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <div style={{ fontWeight: 600, fontSize: 14, marginTop: 22, marginBottom: 8 }}>{children}</div>
);

/** 操作步骤。序号是刻意的：这一页的主线就是"照着做"。 */
const Steps: React.FC<{ items: React.ReactNode[] }> = ({ items }) => (
  <ol style={{ margin: '0 0 12px', paddingLeft: 22, lineHeight: 2 }}>
    {items.map((t, i) => (
      <li key={i}>{t}</li>
    ))}
  </ol>
);

/** 截图。点击可放大（antd Image 自带预览，省得自己写 lightbox）。 */
const Shot: React.FC<{ src: string; caption: string; alt: string }> = ({ src, caption, alt }) => (
  <figure style={{ margin: '14px 0 20px' }}>
    <Image
      src={src}
      alt={alt}
      style={{
        width: '100%',
        borderRadius: 8,
        border: '1px solid var(--yn-border)',
        display: 'block',
      }}
      preview={{ mask: '点击放大' }}
    />
    <figcaption style={{ fontSize: 12, color: '#8a9099', marginTop: 6, textAlign: 'center' }}>
      {caption}
    </figcaption>
  </figure>
);

/** 提示块。`kind` 只区分语气，不改业务含义。 */
const Note: React.FC<{ kind?: 'info' | 'warn'; children: React.ReactNode }> = ({ kind = 'info', children }) => (
  <Alert
    type={kind === 'warn' ? 'warning' : 'info'}
    showIcon
    style={{ margin: '10px 0 16px' }}
    message={<span style={{ fontSize: 13 }}>{children}</span>}
  />
);

const Code: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <code
    className="yn-mono"
    style={{
      background: 'var(--yn-bg)',
      border: '1px solid var(--yn-border)',
      borderRadius: 4,
      padding: '1px 6px',
      fontSize: 12.5,
    }}
  >
    {children}
  </code>
);

// --------------------------------------------------------------------------- 页面

/**
 * 使用文档（一期操作手册）。
 *
 * 定位：<b>教人把事做完</b>，不是讲实现。所以每一节都是"入口在哪 → 点哪个 → 会看到什么 → 看到异常怎么办"，
 * 而不是"这个功能支持哪些字段"。实现细节留在 `docs/01~07` 那套设计文档里。
 *
 * 截图放在 `src/assets/docs/`，必须 `import` —— 本项目的后端 `mountStatic` 只挂
 * `/assets` 与 `/index.html`，`public/` 根目录下的文件在运行时取不到（会返回 index.html，不是 404，
 * 比 404 更难查）。图片已压成 WebP（13 张共约 690KB），否则单文件构建会把 index.html 撑到 4MB。
 *
 * 内容与代码保持一致，几处<b>容易误解的地方</b>（平台最新版本口径、批量发布不可勾选、状态未知）
 * 都在图片旁边直接写了原因，而不是留给用户去猜。
 */
const UsageDocs: React.FC = () => {
  const [active, setActive] = useState('start');

  const nav = useMemo(
    () =>
      SECTIONS.map((s) => (
        <a
          key={s.id}
          href={`#doc-${s.id}`}
          onClick={(e) => {
            // 用 scrollIntoView 而不是靠 href 默认行为：页面滚动容器不是 window，
            // 浏览器的锚点跳转在这个布局里会跳偏。
            e.preventDefault();
            setActive(s.id);
            document.getElementById(`doc-${s.id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
          }}
          style={{
            display: 'block',
            padding: '6px 10px',
            borderRadius: 6,
            fontSize: 13,
            color: active === s.id ? '#1677ff' : '#5b6169',
            background: active === s.id ? '#f0f7ff' : 'transparent',
            textDecoration: 'none',
          }}
        >
          {s.title}
        </a>
      )),
    [active],
  );

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '平台指南' }, { title: '使用文档' }]}
        title="使用文档"
        subTitle="一期操作手册"
        description="按「建镜像 → 发到环境 → 看结果」的主线组织。每节都标明入口位置与操作步骤，截图取自真实环境。"
      />

      <div style={{ display: 'flex', gap: 28, alignItems: 'flex-start' }}>
        {/* 左侧目录：sticky 跟随，长文档里这是唯一能快速定位的东西 */}
        <nav
          style={{
            position: 'sticky',
            top: 16,
            width: 168,
            flex: 'none',
            background: '#fff',
            border: '1px solid var(--yn-border)',
            borderRadius: 10,
            padding: 8,
          }}
        >
          <div style={{ fontSize: 12, color: '#9aa0aa', padding: '4px 10px 8px' }}>目录</div>
          {nav}
        </nav>

        <div style={{ flex: '1 1 auto', minWidth: 0 }}>
          {/* ------------------------------------------------------------ 1 */}
          <Section id="start" title="1. 快速开始" desc="登录进来先看什么，以及首页那几个数字各自的含义。">
            <Sub>1.1 登录</Sub>
            <Paragraph>
              用平台账号登录。<b>能看到哪些菜单由角色权限决定</b> —— 没有
              <Code>env:read</Code> 就看不到「CD 环境部署」，没有 <Code>deploy:approve</Code>
              就看不到审批按钮。看不到某个菜单时，先找管理员确认权限，而不是怀疑页面坏了。
            </Paragraph>
            <Shot src={imgLogin} alt="登录页" caption="登录页：账号密码登录，登录态保存在浏览器本地" />

            <Sub>1.2 概览（首页）</Sub>
            <Paragraph>
              首页是「今天平台整体什么情况」的一屏摘要，数据来自实时接口，右上角有当前时间。
            </Paragraph>
            <Shot src={imgOverview} alt="概览页" caption="概览：纳管环境数、进行中构建、待审批部署、告警服务" />
            <Steps
              items={[
                <>
                  顶部四个数字：<b>纳管环境 / 进行中构建 / 待审批部署 / 告警服务</b>。数字带颜色时说明有事要处理。
                </>,
                <>左侧导航是全部功能入口，按「CI 镜像构建 → CD 环境部署 → AI 助手 → 系统管理」排列。</>,
                <>任何列表页右上角都有「同步」或「自动刷新」，看到的数据旧了点一下即可。</>,
              ]}
            />
          </Section>

          {/* ------------------------------------------------------------ 2 */}
          <Section
            id="ci"
            title="2. CI 镜像构建"
            desc="从「这个项目怎么构建的」到「构建产物在哪」。入口在左侧「CI 镜像构建」展开后的四个菜单。"
          >
            <Sub>2.1 看项目</Sub>
            <Paragraph>
              「项目管理」列出全部可构建的项目。每行给出项目标识、类型、团队、默认分支、构建方式。
              右上角有搜索，可以按项目名或标识过滤。
            </Paragraph>
            <Shot src={imgProjects} alt="项目管理" caption="项目管理：项目标识即 project_key，后面所有地方都用它" />

            <Sub>2.2 项目详情：确认「怎么构建」</Sub>
            <Paragraph>
              点项目名进详情。这一页最该看的是三块：最近一次镜像、构建统计、构建参数。
            </Paragraph>
            <Shot
              src={imgProjectDetail}
              alt="项目详情"
              caption="项目详情：镜像地址、构建耗时、Commit，以及累计构建次数与成功率"
            />
            <Steps
              items={[
                <>
                  「最近镜像版本」给出<b>完整镜像地址</b>和 Digest，右边有 <Code>docker pull</Code>{' '}
                  命令可直接复制。
                </>,
                <>
                  「构建参数」是<b>这个项目保存的构建脚本</b>。改模板不会影响它 —— 要拿新版模板得点「恢复默认脚本」，
                  但那会覆盖你对镜像名、构建目录的自定义，动手前先备份。
                </>,
                <>右上角三个按钮：编辑（改项目配置）、构建历史（只看这个项目的）、编译（发起构建）。</>,
              ]}
            />

            <Sub>2.3 发起一次构建</Sub>
            <Steps
              items={[
                <>在项目详情页点右上角「<b>编译</b>」（列表页也有批量构建入口）。</>,
                <>
                  按构建方式填参数：平台自建构建填分支和 Tag 规则；Jenkins Job 填 Job 参数（下拉候选来自 Jenkins
                  本身，分支多时用搜索）。
                </>,
                <>提交后构建在后台执行，<b>关掉页面不会中断</b>。</>,
                <>状态与日志在「构建记录」里看，进行中的单会自己往下走，列表每 5 秒自动刷新。</>,
              ]}
            />
            <Shot src={imgBuilds} alt="构建记录" caption="构建记录：状态、耗时、触发人，点开可看实时日志" />

            <Sub>2.4 镜像列表</Sub>
            <Paragraph>
              构建成功后产出的镜像都在这。它是<b>平台自己构建过</b>的记录 —— 别人在 Jenkins 或本地推上来的镜像不在这里，
              那种镜像要走「单服务发布 → 选仓库标签」。
            </Paragraph>
            <Shot src={imgImages} alt="镜像列表" caption="镜像列表：平台构建产物的镜像地址、大小、推送时间" />
          </Section>

          {/* ------------------------------------------------------------ 3 */}
          <Section
            id="cd"
            title="3. CD 环境部署"
            desc="发布的主战场。先看拓扑了解全局，再进环境看细节，最后才是发版。"
          >
            <Sub>3.1 发布拓扑：一屏看全链路</Sub>
            <Paragraph>
              选一个项目，就能看到它在<b>每个环境里跑的版本</b>，从左到右按发布链路排列。
              环境之间的箭头表示「下一环」，每个圆圈下面是该环境的实时状态。
            </Paragraph>
            <Shot
              src={imgTopology}
              alt="发布拓扑"
              caption="发布拓扑：机房测试152 → 阿里云预生产 → 阿里云生产，各自当前版本与健康状态"
            />
            <Steps
              items={[
                <>顶部「按发布流水」「按团队分组」切换视图；「图形化视图 / RELEASE TOPOLOGY」两种呈现。</>,
                <>
                  点任意环境圆圈可<b>直接对该服务发起部署、回滚、扩缩容</b> —— 比先进环境列表再找服务快。
                </>,
                <>右上角「批量发布」是后面第 4 节的主角。</>,
                <>链路顺序由环境配置里的「阶段序号」决定，顺序不对去「环境管理 → 编辑环境」调。</>,
              ]}
            />

            <Sub>3.2 环境详情：服务实时状态</Sub>
            <Paragraph>
              点环境名进来。上半部是集群负载，下半部是服务列表（默认标签），另有部署历史、回滚记录、环境变量三个标签。
            </Paragraph>
            <Shot
              src={imgEnvDetail}
              alt="环境详情"
              caption="环境详情：纳管服务数、今日部署、集群负载（含按节点明细）"
            />
            <Shot
              src={imgEnvServices}
              alt="服务列表"
              caption="服务列表：当前镜像版本、副本、负载、状态、重启次数、运行时长"
            />
            <Note>
              这一列的数据<b>直接读集群</b>，不是读平台库。所以「当前镜像版本」旁边写着
              「集群实时 · 平台记录更新于 …」—— 前者是集群里真正在跑的，后者是库里最后一次写入的时间，两者不一致是正常的
              （构建过但还没部署时会差一版）。
            </Note>
            <Steps
              items={[
                <>
                  <b>「重启」列大于 0 时要留意</b>：状态列显示「运行中」时容器也可能在反复重启
                  （<Code>CrashLoopBackOff</Code> / <Code>OOMKilled</Code> 都是这个特征）。重启次数不会说谎。
                </>,
                <>「运行时长」是最新那个 Pod 的年龄，滚动更新后会归零 —— 它说明「当前这版起来多久了」。</>,
                <>
                  搜索框按<b>项目名 / 服务名 / 镜像标签</b>过滤，排查「哪个服务跑着某个版本」时直接搜标签。
                </>,
                <>右上角「自动刷新」默认 5 秒，别处改了状态这里会自己跟上，不用手动 F5。</>,
              ]}
            />

            <Sub>3.3 单服务发布</Sub>
            <Steps
              items={[
                <>环境详情右上角「部署新服务」，或在拓扑页点某个环境节点。</>,
                <>
                  选<b>镜像来源</b>：<b>平台构建</b>（有构建号可溯源）或 <b>仓库标签</b>（Harbor
                  里实际存在的 tag，含别人推的、Jenkins 推的）。
                </>,
                <>确认副本数后提交。环境若开了「部署需审批」，会先进入待审批，审批通过才真正下发。</>,
              ]}
            />
            <Note kind="warn">
              发「仓库标签」这条路径<b>不需要平台构建过</b>，但平台不会替你验证镜像内容 ——
              它会去仓库确认这个 tag 存在并解析 Digest，tag 不存在会当场报错（列表过期时常见）。
            </Note>

            <Sub>3.4 部署记录</Sub>
            <Paragraph>所有部署单都在这。状态、耗时、失败原因、操作人一目了然。</Paragraph>
            <Shot src={imgDeploys} alt="部署记录" caption="部署记录：状态、目标环境、镜像 tag、耗时" />
            <Note>
              状态是「失败」或「已中断」时，如果后面跟着绿色的<b>「事后已就绪」</b>，
              说明集群后来自己好了（镜像最终拉完、Pod 起来了）。<b>这种单不用重发</b> ——
              失败是当时的事实，事后已就绪是现在的事实，两个都要知道。
            </Note>

            <Sub>3.5 回滚</Sub>
            <Paragraph>回滚就是「把某个服务退回它跑过的某个历史版本」。</Paragraph>
            <Shot src={imgRollbacks} alt="回滚记录" caption="回滚记录：谁在什么时候把哪个服务退回了哪个版本" />
            <Steps
              items={[
                <>环境详情「回滚记录」标签，或拓扑页点服务节点。</>,
                <>从该服务的历史版本里挑一个（列表带时间和当时的状态，避免退回到一个本来就有问题的版本）。</>,
                <>回滚同样会生成一张部署单，同样受审批约束，同样能在部署记录里追踪。</>,
              ]}
            />
          </Section>

          {/* ------------------------------------------------------------ 4 */}
          <Section
            id="batch"
            title="4. 批量发布"
            desc="一次把多个服务发到一个环境。三步：选服务 → 核对版本 → 排队发布。这是最容易踩坑的功能，逐屏说明。"
          >
            <Sub>4.1 入口</Sub>
            <Paragraph>
              拓扑页右上角「批量发布」。它会<b>带上当前选中的项目和环境</b>作为默认值，所以先在拓扑页选好，再点这个按钮。
            </Paragraph>
            <Shot
              src={imgBatchDeploy}
              alt="批量发布第一步"
              caption="第 1 步 选择服务：选目标环境，勾要发的服务，右侧「变更」列判断这次会变什么"
            />

            <Sub>4.2 第 1 步：选择服务</Sub>
            <Steps
              items={[
                <>先选<b>目标环境</b>（顶部三个卡片，会标出各自集群与命名空间）。</>,
                <>
                  勾服务。顶部有「全选」「仅选有更新」「清空」三个快捷按钮。
                </>,
                <>
                  看「<b>变更</b>」列决定要不要发：<Tag color="orange">有更新</Tag> 目标版本与当前版本不同；
                  <Tag>已最新</Tag> 相同；<Tag color="orange">需先纳管</Tag> 该服务还没纳管到这个环境；
                  <Tag>无法比对</Tag> 读不到当前版本，<b>不代表没变化</b>。
                </>,
              ]}
            />
            <Note kind="warn">
              「平台最新版本」列读的是<b>平台构建记录</b>，不是镜像仓库。它显示
              <b>「仓库中暂无镜像」</b>时，真实含义是「平台没构建过这个项目」—— 仓库里可能有几十个 tag。
              这类行<b>不可勾选</b>。想发它们有两条路：① 在平台构建一次（产生构建记录）；②
              走单服务发布，在那里选「仓库标签」。
            </Note>

            <Sub>4.3 第 2 步：核对版本</Sub>
            <Paragraph>
              每一行列出「前一环境在跑的版本 / 目标环境当前版本 / 确认发布版本」。第三列可以改：
            </Paragraph>
            <Steps
              items={[
                <>下拉是两组：<b>仓库标签</b>（仓库里真实存在的）和<b>平台构建</b>（带构建号）。选哪个决定走哪条取镜像链路。</>,
                <>
                  <b>「副本」列也可以改</b>。默认取集群实况；清空表示「保持现状不指定」。改过的行会标橙色的「改」字，
                  二次确认弹窗里也会列出来。
                </>,
                <>「同步版本号」把所有行对齐到前一环境正在跑的版本 —— 它<b>不改数据</b>，只改你选的版本。</>,
                <>
                  「重新读取」是<b>整份重来</b>：重新读平台记录、集群实况、仓库标签，并<b>清掉你手改过的版本和副本数</b>。
                  所以顺序是「先重新读取，再逐行调整」，别反过来。
                </>,
              ]}
            />

            <Sub>4.4 第 3 步：排队发布</Sub>
            <Steps
              items={[
                <>确认后一次性建出<b>全部</b>部署单，然后交给后端队列串行执行（默认并发 1）。</>,
                <>
                  <b>这一步建完单就和浏览器无关了</b> —— 关掉页面、断网、换电脑，后面的服务照样会依次发完。
                  不要守在页面上等。
                </>,
                <>第 3 步是只读结果页，轮询只为显示进度，失败不会更改任何状态。真正结果去「部署记录」看。</>,
                <>环境开了「部署需审批」时，生成的是待审批单，审批通过才会下发到集群。</>,
              ]}
            />
            <Note kind="warn">
              同一个服务不允许并发发布。已有进行中的单时，该行会被挡住并显示是哪个单号 ——
              等它结束，别重复提交。
            </Note>
          </Section>

          {/* ------------------------------------------------------------ 5 */}
          <Section
            id="ai"
            title="5. AI 运维助手"
            desc="用自然语言问平台的实时数据。它真的会去查，不是凭记忆答。"
          >
            <Shot src={imgAi} alt="AI 助手" caption="AI 运维助手：右下角输入框提问，回答里带「查询过程」可展开看调了什么工具" />
            <Sub>5.1 能问什么</Sub>
            <Paragraph>
              它挂在 8 个<b>只读</b>工具上，覆盖：环境清单、项目清单、环境内服务清单、单个服务的
              <b>集群实时状态</b>、<b>故障取证</b>（Pod 容器状态 / 退出码 / 事件 / 日志尾部）、部署记录、构建记录、集群资源水位。
            </Paragraph>
            <Steps
              items={[
                <>「pre-aliyun 里 aioitms 现在什么版本、副本够不够？」</>,
                <>「机房测试152 有哪些服务状态不正常？」</>,
                <>「aiouser 为什么起不来？」—— 它会去取 Pod 事件和容器日志尾部，而不是猜。</>,
                <>「集群里有没有 xxljobadmin 这个服务？」—— 未纳管的也能查，回答里会标出「集群有但平台没纳管」。</>,
              ]}
            />
            <Note>
              每次提问的第<b>一轮</b>都会强制调用工具去查数据，所以你看到回答前会先出现「查询过程 · N 次」。
              如果一个回答没有任何查询过程，那它就不该被当作事实 —— 直接追问「你去查了吗」。
            </Note>
            <Note kind="warn">
              它<b>只能读</b>。让它「帮我发一版」「重启一下」它不会照做，只会告诉你入口在哪。
              这是刻意设计：写操作必须有人按下那个按钮。
            </Note>
          </Section>

          {/* ------------------------------------------------------------ 6 */}
          <Section id="status" title="6. 状态含义对照" desc="几个容易混淆的标签，以及它们出现时可不可以继续操作。">
            <Table
              size="small"
              pagination={false}
              rowKey="k"
              style={{ marginBottom: 8 }}
              columns={[
                { title: '标签', dataIndex: 'k', width: 150 },
                { title: '含义', dataIndex: 'v' },
                { title: '该怎么做', dataIndex: 'a', width: 260 },
              ]}
              dataSource={[
                { k: '运行中', v: '集群里工作负载就绪，副本数达标', a: '不用管；但顺手看一眼「重启」列' },
                { k: '部署中', v: '平台正在下发并等待就绪', a: '等；长时间不动去部署记录看失败原因' },
                { k: '状态未知', v: '这次没读到集群（网络 / 凭据 / 超时），不是"服务挂了"', a: '点同步重试；持续未知查环境详情里的原因' },
                { k: '有更新', v: '目标版本与集群当前版本不同', a: '确认版本无误即可发布' },
                { k: '已最新', v: '目标版本与当前版本相同', a: '不需要发；发也是同版本重发' },
                { k: '需先纳管', v: '该服务没有纳管到这个环境', a: '先在环境管理里纳管，集群负载不会动' },
                { k: '无法比对', v: '读不到集群当前版本，判断不了有没有变化', a: '不代表没更新，先解决读取问题' },
                { k: '事后已就绪', v: '判失败/中断之后，集群自己变好了', a: '这种单不用重发' },
                { k: '已中断', v: '平台重启打断了观察，集群侧动作可能仍在继续', a: '先看有无「事后已就绪」' },
                { k: '待审批', v: '环境开了发布审批，单还没被批准', a: '找审批人；不会产生执行日志' },
              ]}
            />
          </Section>

          {/* ------------------------------------------------------------ 7 */}
          <Section id="faq" title="7. 常见问题" desc="按「遇到什么现象 → 先看哪里 → 怎么处理」组织。">
            <Sub>列表里同一个服务，两个环境显示的版本不一样？</Sub>
            <Paragraph>
              正常。这是「各环境集群里真正在跑的」版本，滚动更新推进到哪一版就是哪一版。
              想看链路整体齐不齐，去「发布拓扑」。
            </Paragraph>

            <Sub>批量发布时有些服务勾不上，复选框是灰的？</Sub>
            <Paragraph>
              三种原因：<b>没纳管到这个环境</b>、<b>已有进行中的部署单</b>、<b>平台没有构建记录</b>。
              前两种看行内提示；第三种就是上面 4.2 说的「仓库中暂无镜像」，去平台构建一次，或走单服务发布选仓库标签。
            </Paragraph>

            <Sub>构建成功了，但服务器上没看到新镜像？</Sub>
            <Paragraph>
              构建成功 ≠ 推送成功。去「构建记录」点开那次构建，看最后一段日志有没有
              <Code>The push refers to repository</Code> 和 Digest。<b>构建成功只代表产物打出来了。</b>
            </Paragraph>

            <Sub>部署一直卡在「部署中」？</Sub>
            <Paragraph>
              先看这个服务的「重启」列有没有涨，再看 Pod 状态：镜像拉不下来（
              <Code>ImagePullBackOff</Code>）和容器起不来（<Code>CrashLoopBackOff</Code>）都表现为「一直是部署中」。
              AI 助手问一句「X 为什么起不来」，它会把容器状态、退出码、事件和日志尾部一起拿出来。
            </Paragraph>

            <Sub>环境整体「状态未知」？</Sub>
            <Paragraph>
              说明平台连不上那个集群，<b>不是服务出了问题</b>。按顺序查：① 点「同步」重试；②
              去「系统管理 → 集群接入」看该集群的探测结果；③ 在同网段机器上
              <Code>ping</Code> 一下 API Server 地址看丢包。网络慢到几秒才能建连时，读取会撞超时，全部显示未知。
            </Paragraph>

            <Sub>为什么别人能看到某个菜单/按钮，我看不到？</Sub>
            <Paragraph>
              权限不同。菜单和按钮都按权限点渲染（如 <Code>env:read</Code>、<Code>deploy:approve</Code>），
              找管理员在「系统管理 → 角色」里调整即可，不是页面 bug。
            </Paragraph>
          </Section>

          <div style={{ borderTop: '1px solid var(--yn-border)', paddingTop: 14, color: '#9aa0aa', fontSize: 12 }}>
            一期操作手册 · 截图取自真实环境 · 若页面与文档不一致，以页面为准并反馈给平台维护者
          </div>
        </div>
      </div>
    </div>
  );
};

export default UsageDocs;
