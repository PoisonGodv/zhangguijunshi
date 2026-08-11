// 微信小程序运行时由 AppService 注入的全局对象引用。
// miniprogram-api-typings 未声明 `global`,业务代码(app.ts 埋点劫持 Page)需要,
// 故在此补最小声明(仅用于 TS 类型检查,noEmit 不产线产物)。
declare const global: any
