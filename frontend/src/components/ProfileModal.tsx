import React, { useEffect, useState } from 'react';
import { Alert, App as AntApp, Avatar, Button, Form, Input, Modal, Space, Tabs, Typography, Upload } from 'antd';
import { CameraOutlined, UserOutlined } from '@ant-design/icons';
import { authApi } from '@/api';
import { getSession, refreshMe } from '@/auth';

const { Text } = Typography;

export interface ProfileModalProps {
  open: boolean;
  onClose: () => void;
  /** 保存成功后通知外层重新读一次会话（头像/姓名变了要立刻反映在头部） */
  onSaved?: () => void;
}

/**
 * 个人设置。
 *
 * 三块：头像、基本信息、改密码。
 *
 * ⚠️ 这里**只能改自己**的姓名/邮箱/电话/头像 —— 角色与启用状态不在这里（那是管理员的事）。
 * 界面上不显示那些字段，比显示了再置灰更清楚：不给出来的东西，用户不会以为自己能改。
 */
const ProfileModal: React.FC<ProfileModalProps> = ({ open, onClose, onSaved }) => {
  const { message } = AntApp.useApp();
  const me = getSession();

  const [tab, setTab] = useState('profile');
  const [avatar, setAvatar] = useState(me?.avatar ?? '');
  const [uploading, setUploading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [pwdSaving, setPwdSaving] = useState(false);

  const [form] = Form.useForm();
  const [pwdForm] = Form.useForm();

  useEffect(() => {
    if (!open) return;
    setAvatar(me?.avatar ?? '');
    setTab('profile');
    form.setFieldsValue({ name: me?.name ?? '', email: me?.email ?? '', phone: me?.phone ?? '' });
    pwdForm.resetFields();
  }, [open, me?.name, me?.email, me?.phone, me?.avatar, form, pwdForm]);

  /** 读成 dataURL 再交给后端：http 封层固定发 JSON，multipart 得绕开它 */
  const pickAvatar = (file: File) => {
    if (file.size > 1024 * 1024) {
      message.error('头像不能超过 1MB');
      return;
    }
    setUploading(true);
    const reader = new FileReader();
    reader.onload = async () => {
      try {
        const res = await authApi.uploadAvatar(file.name, String(reader.result ?? ''));
        setAvatar(res.avatar);
        await refreshMe();
        onSaved?.();
        message.success('头像已更新');
      } catch (e) {
        message.error((e as Error)?.message ?? '上传头像失败');
      } finally {
        setUploading(false);
      }
    };
    reader.onerror = () => {
      setUploading(false);
      message.error('读取本地文件失败');
    };
    reader.readAsDataURL(file);
  };

  const saveProfile = async () => {
    const v = await form.validateFields();
    setSaving(true);
    try {
      await authApi.updateSelf({ name: v.name, email: v.email, phone: v.phone });
      await refreshMe();
      onSaved?.();
      message.success('已保存');
    } catch (e) {
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const savePassword = async () => {
    const v = await pwdForm.validateFields();
    setPwdSaving(true);
    try {
      await authApi.changePassword(v.oldPassword, v.newPassword);
      pwdForm.resetFields();
      message.success('密码已修改，下次登录请使用新密码');
    } catch (e) {
      message.error((e as Error)?.message ?? '修改密码失败');
    } finally {
      setPwdSaving(false);
    }
  };

  return (
    <Modal open={open} onCancel={onClose} footer={null} width={560} title="个人设置" destroyOnHidden>
      <Tabs
        activeKey={tab}
        onChange={setTab}
        items={[
          {
            key: 'profile',
            label: '基本信息',
            children: (
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 16, marginBottom: 22 }}>
                  <Avatar size={64} src={avatar || undefined} style={{ background: '#1677ff', fontSize: 24 }}>
                    {(me?.name || me?.username || '?').slice(0, 1)}
                  </Avatar>
                  <Space direction="vertical" size={2}>
                    <Upload
                      showUploadList={false}
                      accept="image/png,image/jpeg,image/webp"
                      beforeUpload={(file) => {
                        pickAvatar(file as File);
                        // 返回 false：不让 antd 自己上传，我们走 base64 JSON
                        return false;
                      }}
                    >
                      <Button size="small" icon={<CameraOutlined />} loading={uploading}>
                        {avatar ? '更换头像' : '上传头像'}
                      </Button>
                    </Upload>
                    <Text type="secondary" style={{ fontSize: 11.5 }}>
                      png / jpg / webp，不超过 1MB
                    </Text>
                  </Space>
                </div>

                <Form form={form} layout="vertical" requiredMark={false}>
                  <Form.Item label="姓名" name="name" rules={[{ required: true, message: '请填写姓名' }]}>
                    <Input placeholder="显示在界面与审计记录里的名字" maxLength={64} />
                  </Form.Item>
                  <Form.Item
                    label="邮箱"
                    name="email"
                    rules={[{ type: 'email', message: '邮箱格式不正确' }]}
                    extra="用于接收系统通知；留空表示没有邮箱"
                  >
                    <Input placeholder="name@example.com" maxLength={128} />
                  </Form.Item>
                  <Form.Item label="手机号" name="phone" extra="仅作联系方式记录，暂不用于登录">
                    <Input placeholder="选填" maxLength={20} />
                  </Form.Item>
                  <Form.Item label="登录账号">
                    {/* 账号是身份标识，不在这里改：改了会话与审计里的历史就断了 */}
                    <Input value={me?.username ?? ''} disabled />
                  </Form.Item>
                </Form>

                <Space>
                  <Button type="primary" loading={saving} onClick={saveProfile}>
                    保存
                  </Button>
                  <Button onClick={onClose}>取消</Button>
                </Space>
              </div>
            ),
          },
          {
            key: 'password',
            label: '修改密码',
            children: (
              <div>
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 16 }}
                  message="修改后当前会话仍然有效"
                  description={
                    <span style={{ fontSize: 12 }}>
                      密码只用于下次登录。若怀疑账号被盗用，改完密码后请让管理员停用并重新启用你的账号，
                      那会吊销所有已签发的令牌。
                    </span>
                  }
                />
                <Form form={pwdForm} layout="vertical" requiredMark={false}>
                  <Form.Item label="当前密码" name="oldPassword" rules={[{ required: true, message: '请输入当前密码' }]}>
                    <Input.Password autoComplete="current-password" />
                  </Form.Item>
                  <Form.Item
                    label="新密码"
                    name="newPassword"
                    rules={[
                      { required: true, message: '请输入新密码' },
                      { min: 8, message: '至少 8 位' },
                    ]}
                  >
                    <Input.Password autoComplete="new-password" />
                  </Form.Item>
                  <Form.Item
                    label="确认新密码"
                    name="confirm"
                    dependencies={['newPassword']}
                    rules={[
                      { required: true, message: '请再输入一次' },
                      ({ getFieldValue }) => ({
                        validator: (_, v) =>
                          !v || v === getFieldValue('newPassword')
                            ? Promise.resolve()
                            : Promise.reject(new Error('两次输入的密码不一致')),
                      }),
                    ]}
                  >
                    <Input.Password autoComplete="new-password" />
                  </Form.Item>
                </Form>
                <Space>
                  <Button type="primary" loading={pwdSaving} onClick={savePassword}>
                    修改密码
                  </Button>
                  <Button onClick={() => pwdForm.resetFields()}>重置</Button>
                </Space>
              </div>
            ),
          },
        ]}
      />
    </Modal>
  );
};

export default ProfileModal;
