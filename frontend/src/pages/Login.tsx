import React, { useEffect, useState } from 'react';
import { App as AntApp, Button, Checkbox, Form, Input, Space } from 'antd';
import {
  BranchesOutlined,
  CloudServerOutlined,
  DeploymentUnitOutlined,
  LockOutlined,
  LoginOutlined,
  RobotOutlined,
  SafetyCertificateOutlined,
  ThunderboltOutlined,
  UserOutlined,
} from '@ant-design/icons';
import { useLocation, useNavigate } from 'react-router-dom';
import { ApiError } from '@/api';
import { isLoggedIn, login, wasRemembered } from '@/auth';

// 验证码字符集：去掉容易混淆的 0/O、1/I/L
const CAPTCHA_CHARS = 'ABCDEFGHJKMNPQRSTUVWXYZ23456789';

const genCaptcha = (): string =>
  Array.from({ length: 4 }, () => CAPTCHA_CHARS[Math.floor(Math.random() * CAPTCHA_CHARS.length)]).join('');

/**
 * 品牌区的四个数字是**静态展示位**，不是实时数据。
 * 登录前页面拿不到任何业务接口（全部要求登录态），所以这里不接后端；
 * 登录后的真实统计在概览页（/system/stats）。
 */
const BRAND_STATS = [
  { value: '20', label: '纳管项目' },
  { value: '5', label: '运行环境' },
  { value: '60', label: '服务实例' },
  { value: '98', label: '构建成功率%' },
];

interface LoginForm {
  username: string;
  password: string;
  captcha: string;
  remember: boolean;
}

const Login: React.FC = () => {
  const { message } = AntApp.useApp();
  const navigate = useNavigate();
  const location = useLocation();
  const [form] = Form.useForm<LoginForm>();
  const [captcha, setCaptcha] = useState<string>(genCaptcha);
  const [loading, setLoading] = useState(false);
  const [shake, setShake] = useState(false);

  /** 路由守卫重定向过来时带上的原目标地址，登录后跳回去 */
  const from = (location.state as { from?: string } | null)?.from;

  // 已登录用户直接进控制台，不需要再看登录页
  useEffect(() => {
    if (isLoggedIn()) navigate('/overview', { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const fail = (msg: string) => {
    setCaptcha(genCaptcha());
    setShake(true);
    window.setTimeout(() => setShake(false), 460);
    message.error(msg);
  };

  const onFinish = async (v: LoginForm) => {
    if (v.captcha.trim().toUpperCase() !== captcha) {
      fail('验证码不正确，已刷新，请重新输入');
      return;
    }
    setLoading(true);
    try {
      const user = await login(v.username.trim(), v.password, v.remember);
      message.success(`欢迎回来，${user.name}`);
      navigate(from && from !== '/login' ? from : '/overview', { replace: true });
    } catch (e) {
      // 后端已经把失败原因写进 message（如"账号或密码错误"），直接透传比前端再猜一遍准确
      fail(e instanceof ApiError ? e.message : '登录失败，请稍后重试');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="yn-login">
      {/* ------------------------------------------------------------------ 左侧品牌区 */}
      <div className="yn-login__brand">
        <div className="yn-login__grid" />
        <div className="yn-login__scan" />
        <span className="yn-login__orb yn-login__orb--a" />
        <span className="yn-login__orb yn-login__orb--b" />
        <span className="yn-login__orb yn-login__orb--c" />
        <span className="yn-login__orb yn-login__orb--d" />

        <div className="yn-login__brand-inner">
          <div className="yn-login__logo">
            <span className="yn-login__logo-mark">云</span>
            <span>
              <div className="yn-login__logo-name">YunNian 云辇</div>
              <div className="yn-login__logo-sub">AI 运维管理发布平台</div>
            </span>
          </div>

          <h1 className="yn-login__headline">
            让 <em>AI</em> 驱动每一次构建与发布，
            <br />
            上线都<em>有据可查</em>
          </h1>
          <div className="yn-login__sub">
            统一纳管 CI 镜像构建与 CD 环境部署，
            <br />
            AI 贯穿从代码提交到多环境上线的全链路闭环。
          </div>

          <div className="yn-login__points">
            <div className="yn-login__point">
              <span className="yn-login__point-icon">
                <ThunderboltOutlined />
              </span>
              <span>
                AI 预填构建发布
                <em>按历史成功记录自动预填参数，批量打包与批量发布排队执行</em>
              </span>
            </div>
            <div className="yn-login__point">
              <span className="yn-login__point-icon">
                <DeploymentUnitOutlined />
              </span>
              <span>
                AI 解读拓扑日志
                <em>圆圈拓扑一眼看清各环境版本差异，日志异常由 AI 直接点出</em>
              </span>
            </div>
            <div className="yn-login__point">
              <span className="yn-login__point-icon">
                <RobotOutlined />
              </span>
              <span>
                AI 归因与风险预判
                <em>构建失败自动定位原因，发布前评估版本差异与回滚可行性</em>
              </span>
            </div>
          </div>
        </div>

        <div className="yn-login__stats">
          {BRAND_STATS.map((s) => (
            <div className="yn-login__stat" key={s.label}>
              <b>{s.value}</b>
              <span>{s.label}</span>
            </div>
          ))}
        </div>
      </div>

      {/* ------------------------------------------------------------------ 右侧登录区 */}
      <div className="yn-login__panel">
        <div className={`yn-login__card${shake ? ' yn-login__card--shake' : ''}`}>
          <h2 className="yn-login__title">登录控制台</h2>
          <div className="yn-login__title-sub">
            <span>使用平台账号登录，账号由管理员统一分配</span>
            <span className="yn-login__pulse">
              <i />
              服务正常
            </span>
          </div>

          <Form
            form={form}
            layout="vertical"
            requiredMark={false}
            initialValues={{
              username: '',
              password: '',
              captcha: '',
              remember: wasRemembered(),
            }}
            onFinish={onFinish}
          >
            <Form.Item
              name="username"
              label="账号"
              rules={[{ required: true, message: '请输入账号' }]}
              style={{ marginBottom: 16 }}
            >
              <Input size="large" prefix={<UserOutlined style={{ color: '#b5bac4' }} />} placeholder="用户名 / 邮箱" />
            </Form.Item>

            <Form.Item
              name="password"
              label="密码"
              rules={[{ required: true, message: '请输入密码' }]}
              style={{ marginBottom: 16 }}
            >
              <Input.Password
                size="large"
                prefix={<LockOutlined style={{ color: '#b5bac4' }} />}
                placeholder="密码"
                autoComplete="current-password"
              />
            </Form.Item>

            <div className="yn-login__row">
              <Form.Item
                name="captcha"
                label="验证码"
                rules={[{ required: true, message: '请输入验证码' }]}
                style={{ marginBottom: 0 }}
              >
                <Input
                  size="large"
                  prefix={<SafetyCertificateOutlined style={{ color: '#b5bac4' }} />}
                  placeholder="不区分大小写"
                  maxLength={4}
                />
              </Form.Item>
              <div style={{ paddingTop: 30 }}>
                <div
                  className="yn-login__captcha"
                  title="点击刷新验证码"
                  onClick={() => {
                    setCaptcha(genCaptcha());
                    form.setFieldValue('captcha', '');
                  }}
                >
                  <span>{captcha}</span>
                </div>
              </div>
            </div>

            <div className="yn-login__meta" style={{ marginTop: 16 }}>
              <Form.Item name="remember" valuePropName="checked" noStyle>
                <Checkbox>记住我（7 天）</Checkbox>
              </Form.Item>
              <Button type="link" size="small" style={{ padding: 0 }} onClick={() => message.info('请联系管理员重置密码（原型演示）')}>
                忘记密码？
              </Button>
            </div>

            <Form.Item style={{ marginBottom: 0 }}>
              <Button
                type="primary"
                size="large"
                block
                htmlType="submit"
                loading={loading}
                icon={<LoginOutlined />}
              >
                {loading ? '正在验证身份…' : '登 录'}
              </Button>
            </Form.Item>
          </Form>

          <div className="yn-login__alt">其他登录方式</div>
          <div className="yn-login__sso">
            <Button block icon={<BranchesOutlined />} onClick={() => message.info('企业微信扫码登录（原型演示）')}>
              企业微信
            </Button>
            <Button block icon={<CloudServerOutlined />} onClick={() => message.info('统一认证 SSO / OIDC（原型演示）')}>
              统一认证
            </Button>
          </div>

          <div className="yn-login__foot">
            <Space size={6} split={<span style={{ color: '#e0e3e9' }}>|</span>}>
              <span>YunNian 云辇 v0.1.0-mvp</span>
              <span>内网演示环境</span>
            </Space>
            <div style={{ marginTop: 4 }}>登录态由 JWT 承载，会话可由服务端随时吊销</div>
          </div>
        </div>
      </div>
    </div>
  );
};

export default Login;
