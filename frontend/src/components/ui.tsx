import React from 'react';
import { Breadcrumb, Button, Card, Space, Typography } from 'antd';
import { ArrowDownOutlined, ArrowUpOutlined } from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { AnimatedNumber } from './AnimatedNumber';

const { Text, Title } = Typography;

// --------------------------------------------------------------------------- 页面标题区（对标阿里云控制台页头）

export interface PageHeaderProps {
  breadcrumb?: { title: string; path?: string }[];
  title: React.ReactNode;
  subTitle?: React.ReactNode;
  description?: React.ReactNode;
  extra?: React.ReactNode;
  tags?: React.ReactNode;
  onBack?: () => void;
}

export const PageHeader: React.FC<PageHeaderProps> = ({
  breadcrumb,
  title,
  subTitle,
  description,
  extra,
  tags,
  onBack,
}) => (
  <div className="yn-page-header">
    {breadcrumb?.length ? (
      <Breadcrumb
        style={{ marginBottom: 8 }}
        items={breadcrumb.map((b, i) => ({
          title:
            b.path && i < breadcrumb.length - 1 ? (
              <Link to={b.path}>{b.title}</Link>
            ) : (
              <span>{b.title}</span>
            ),
        }))}
      />
    ) : null}
    <div className="yn-page-header__main">
      <div>
        <Space size={10} align="center" wrap>
          {onBack ? (
            <Button type="text" size="small" onClick={onBack} style={{ paddingInline: 4 }}>
              返回
            </Button>
          ) : null}
          <Title level={4} style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>
            {title}
          </Title>
          {subTitle ? (
            <Text type="secondary" className="yn-mono" style={{ fontSize: 13 }}>
              {subTitle}
            </Text>
          ) : null}
          {tags}
        </Space>
        {description ? (
          <div style={{ marginTop: 6, color: '#70757f', fontSize: 13 }}>{description}</div>
        ) : null}
      </div>
      {extra ? <div className="yn-page-header__extra">{extra}</div> : null}
    </div>
  </div>
);

// --------------------------------------------------------------------------- 指标卡

export interface StatCardProps {
  title: string;
  value: React.ReactNode;
  unit?: string;
  delta?: number;
  deltaLabel?: string;
  /** 同比上涨是"好事"还是"坏事"，影响配色（如失败率上涨为红） */
  positiveIsGood?: boolean;
  icon?: React.ReactNode;
  accent?: string;
  footer?: React.ReactNode;
}

export const StatCard: React.FC<StatCardProps> = ({
  title,
  value,
  unit,
  delta,
  deltaLabel = '较昨日',
  positiveIsGood = true,
  icon,
  accent = '#1677ff',
  footer,
}) => {
  const up = (delta ?? 0) >= 0;
  const good = up === positiveIsGood;
  return (
    <Card className="yn-stat-card" variant="borderless" styles={{ body: { padding: '18px 20px' } }}>
      <div className="yn-stat-card__top">
        <span className="yn-stat-card__title">{title}</span>
        {icon ? <span className="yn-stat-card__icon" style={{ color: accent }}>{icon}</span> : null}
      </div>
      <div className="yn-stat-card__value" style={{ color: accent }}>
        {/* 纯数字指标走滚动动画；「20 / 29」这类复合值原样渲染 */}
        {typeof value === 'number' ? <AnimatedNumber value={value} /> : value}
        {unit ? <em className="yn-stat-card__unit">{unit}</em> : null}
      </div>
      <div className="yn-stat-card__footer">
        {delta === undefined ? (
          footer
        ) : (
          <Space size={6}>
            <span className={good ? 'yn-delta-good' : 'yn-delta-bad'}>
              {up ? <ArrowUpOutlined /> : <ArrowDownOutlined />}
              {Math.abs(delta)}%
            </span>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {deltaLabel}
            </Text>
          </Space>
        )}
      </div>
    </Card>
  );
};

// --------------------------------------------------------------------------- 内容卡片

export interface SectionCardProps {
  title?: React.ReactNode;
  extra?: React.ReactNode;
  children: React.ReactNode;
  bodyStyle?: React.CSSProperties;
  style?: React.CSSProperties;
  className?: string;
}

export const SectionCard: React.FC<SectionCardProps> = ({
  title,
  extra,
  children,
  bodyStyle,
  style,
  className,
}) => (
  <Card
    className={`yn-section-card ${className ?? ''}`}
    variant="borderless"
    style={style}
    styles={{ body: { padding: 0, ...bodyStyle } }}
    title={
      title ? (
        <div className="yn-section-card__title">
          <span>{title}</span>
          {extra ? <span style={{ fontWeight: 400 }}>{extra}</span> : null}
        </div>
      ) : undefined
    }
  >
    {children}
  </Card>
);

// --------------------------------------------------------------------------- 键值对展示

export const KVList: React.FC<{ data: { label: string; value: React.ReactNode }[]; columns?: number }> = ({
  data,
  columns = 3,
}) => (
  <div className="yn-kv" style={{ gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }}>
    {data.map((d, i) => (
      <div className="yn-kv__item" key={i}>
        <div className="yn-kv__label">{d.label}</div>
        <div className="yn-kv__value">{d.value}</div>
      </div>
    ))}
  </div>
);
