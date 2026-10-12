import React from 'react';
import ReactDOM from 'react-dom/client';
import { App as AntApp, ConfigProvider, theme as antdTheme } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import dayjs from 'dayjs';
import 'dayjs/locale/zh-cn';
import App from './App';
import './styles/global.css';
import './styles/topology.css';
import './styles/batch.css';
import './styles/login.css';
import './styles/motion.css';

dayjs.locale('zh-cn');

// 阿里云控制台风格：主色蓝、圆角 6、字号 14、组件紧凑
const themeConfig = {
  algorithm: antdTheme.defaultAlgorithm,
  token: {
    colorPrimary: '#1677ff',
    colorLink: '#1677ff',
    colorSuccess: '#52c41a',
    colorWarning: '#faad14',
    colorError: '#ff4d4f',
    colorText: '#2c313a',
    colorTextSecondary: '#5c6470',
    colorBgLayout: '#f4f6f9',
    borderRadius: 6,
    fontSize: 14,
    controlHeight: 32,
    wireframe: false,
  },
  components: {
    Menu: {
      itemHeight: 38,
      itemBorderRadius: 6,
      horizontalItemSelectedColor: '#1677ff',
      activeBarHeight: 2,
    },
    Table: {
      headerBg: '#fafbfc',
      headerColor: '#5c6470',
      borderColor: '#eceef2',
      cellPaddingBlock: 10,
      cellPaddingInline: 12,
    },
    Card: {
      headerHeight: 46,
      headerFontSize: 14,
    },
    Layout: {
      headerBg: '#ffffff',
      headerHeight: 56,
      headerPadding: '0 20px',
      siderBg: '#ffffff',
    },
    Tabs: {
      horizontalItemPadding: '10px 0',
      cardBg: '#fafbfc',
    },
    Drawer: {
      paddingLG: 20,
    },
    Descriptions: {
      labelBg: '#fafbfc',
      titleMarginBottom: 12,
    },
    Segmented: {
      itemSelectedBg: '#ffffff',
    },
  },
};

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ConfigProvider locale={zhCN} theme={themeConfig}>
      <AntApp>
        <App />
      </AntApp>
    </ConfigProvider>
  </React.StrictMode>,
);
