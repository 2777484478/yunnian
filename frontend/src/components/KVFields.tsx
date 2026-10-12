import React from 'react';
import { Button, Form, Input, Space, Switch, Typography } from 'antd';
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * Key-Value 参数编辑器（构建参数 / 环境变量 / 部署参数通用）
 * 用法：<Form.Item name="buildParams"><KVFields /></Form.Item> 或
 *      <KVFields name="buildParams" /> （内部自带 Form.List）
 */
export const KVFields: React.FC<{
  name: string;
  /** 是否展示"密文"开关 */
  allowSecret?: boolean;
  addText?: string;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
}> = ({ name, allowSecret = true, addText = '添加参数', keyPlaceholder = '如 NPM_REGISTRY', valuePlaceholder = '参数值' }) => (
  <Form.List name={name}>
    {(fields, { add, remove }) => (
      <div className="yn-kvfields">
        {fields.map(({ key, name: fieldName, ...rest }) => (
          <Space key={key} align="baseline" style={{ display: 'flex', marginBottom: 8 }} size={8}>
            <Form.Item {...rest} name={[fieldName, 'key']} rules={[{ required: true, message: '请输入 Key' }]} style={{ marginBottom: 0 }}>
              <Input placeholder={keyPlaceholder} style={{ width: 210 }} />
            </Form.Item>
            <span className="yn-kveq">=</span>
            <Form.Item {...rest} name={[fieldName, 'value']} style={{ marginBottom: 0 }}>
              <Input placeholder={valuePlaceholder} style={{ width: 300 }} />
            </Form.Item>
            <Form.Item {...rest} name={[fieldName, 'secret']} valuePropName="checked" style={{ marginBottom: 0 }}>
              <Switch size="small" checkedChildren="密" unCheckedChildren="明" />
            </Form.Item>
            <Button type="text" danger icon={<DeleteOutlined />} onClick={() => remove(fieldName)} />
          </Space>
        ))}
        <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ key: '', value: '', secret: false })} block>
          {addText}
        </Button>
        {!allowSecret ? null : (
          <Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 6 }}>
            标记为「密」的参数在列表与日志中会脱敏展示；建议密钥类参数走平台密钥管理。
          </Text>
        )}
      </div>
    )}
  </Form.List>
);
