import packageJson from "../../package.json";

const appName = "CTEST";
const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: appName,
  version: packageJson.version,
  copyright: `© ${currentYear}, ${appName}.`,
  meta: {
    title: appName,
    description: "LLM 驱动的自主渗透测试系统控制台",
  },
};
