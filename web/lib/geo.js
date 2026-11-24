// Locations from vast look like "Nevada, US" or "Switzerland, CH": a region
// (or the country's English name, or nothing) and an ISO 3166 country code.
// fmtGeo shows them in the mode picked at the bottom of the machine list.

const MODE_KEY = 'vastmon.geoMode';

export const GEO_MODES = {
  raw: '原文',
  zh: '中文',
  continent: '中文［国家＋大洲］',
};

export function geoMode() {
  const m = localStorage.getItem(MODE_KEY);
  return Object.prototype.hasOwnProperty.call(GEO_MODES, m) ? m : 'zh';
}

export function setGeoMode(m) {
  localStorage.setItem(MODE_KEY, m);
}

const CONTINENTS = {
  亚洲: 'AF AM AZ BH BD BT BN KH CN CY GE HK IN ID IR IQ IL IO JP JO KZ KW KG LA LB MO MY MV MN MM NP KP OM PK PS PH QA SA SG KR LK SY TW TJ TH TL TR TM AE UZ VN YE',
  欧洲: 'AL AD AT AX BY BE BA BG HR CZ DK EE FO FI FR DE GI GR GG HU IS IE IM IT JE XK LV LI LT LU MT MD MC ME NL MK NO PL PT RO RU SM RS SK SI ES SJ SE CH UA GB VA',
  北美洲: 'AI AG AW BS BB BZ BM BQ VG CA KY CR CU CW DM DO SV GL GD GP GT HT HN JM MQ MX MS NI PA PR BL KN LC MF PM VC SX TT TC US VI UM',
  南美洲: 'AR BO BR CL CO EC FK GF GY PY PE SR UY VE',
  非洲: 'DZ AO BJ BW BF BI CV CM CF TD KM CG CD CI DJ EG GQ ER SZ ET GA GM GH GN GW KE LS LR LY MG MW ML MR MU YT MA MZ NA NE NG RE RW SH ST SN SC SL SO ZA SS SD TZ TG TN UG EH ZM ZW',
  大洋洲: 'AS AU CK FJ PF GU KI MH FM NR NC NZ NU NF MP PW PG PN WS SB TK TO TV VU WF',
  南极洲: 'AQ BV GS HM TF',
};
const CONTINENT_OF = {};
for (const [name, codes] of Object.entries(CONTINENTS)) {
  for (const c of codes.split(' ')) CONTINENT_OF[c] = name;
}

// Short names where the browser's are long or differ from common use.
const COUNTRY_ZH = { HK: '香港', MO: '澳门', TW: '台湾', AE: '阿联酋' };

const REGIONS_ZH = {
  US: {
    Alabama: '阿拉巴马州', Alaska: '阿拉斯加州', Arizona: '亚利桑那州', Arkansas: '阿肯色州',
    California: '加利福尼亚州', Colorado: '科罗拉多州', Connecticut: '康涅狄格州', Delaware: '特拉华州',
    'District of Columbia': '华盛顿特区', Florida: '佛罗里达州', Georgia: '佐治亚州', Hawaii: '夏威夷州',
    Idaho: '爱达荷州', Illinois: '伊利诺伊州', Indiana: '印第安纳州', Iowa: '艾奥瓦州', Kansas: '堪萨斯州',
    Kentucky: '肯塔基州', Louisiana: '路易斯安那州', Maine: '缅因州', Maryland: '马里兰州',
    Massachusetts: '马萨诸塞州', Michigan: '密歇根州', Minnesota: '明尼苏达州', Mississippi: '密西西比州',
    Missouri: '密苏里州', Montana: '蒙大拿州', Nebraska: '内布拉斯加州', Nevada: '内华达州',
    'New Hampshire': '新罕布什尔州', 'New Jersey': '新泽西州', 'New Mexico': '新墨西哥州', 'New York': '纽约州',
    'North Carolina': '北卡罗来纳州', 'North Dakota': '北达科他州', Ohio: '俄亥俄州', Oklahoma: '俄克拉何马州',
    Oregon: '俄勒冈州', Pennsylvania: '宾夕法尼亚州', 'Rhode Island': '罗得岛州', 'South Carolina': '南卡罗来纳州',
    'South Dakota': '南达科他州', Tennessee: '田纳西州', Texas: '得克萨斯州', Utah: '犹他州', Vermont: '佛蒙特州',
    Virginia: '弗吉尼亚州', Washington: '华盛顿州', 'West Virginia': '西弗吉尼亚州', Wisconsin: '威斯康星州',
    Wyoming: '怀俄明州',
  },
  CA: {
    Alberta: '艾伯塔省', 'British Columbia': '不列颠哥伦比亚省', Manitoba: '马尼托巴省',
    'New Brunswick': '新不伦瑞克省', 'Newfoundland and Labrador': '纽芬兰与拉布拉多省', 'Nova Scotia': '新斯科舍省',
    Ontario: '安大略省', 'Prince Edward Island': '爱德华王子岛省', Quebec: '魁北克省', Saskatchewan: '萨斯喀彻温省',
    Yukon: '育空', 'Northwest Territories': '西北地区', Nunavut: '努纳武特',
  },
  CN: {
    Anhui: '安徽', Beijing: '北京', Chongqing: '重庆', Fujian: '福建', Gansu: '甘肃', Guangdong: '广东',
    Guangxi: '广西', 'Guangxi Zhuangzu': '广西', Guizhou: '贵州', Hainan: '海南', Hebei: '河北',
    Heilongjiang: '黑龙江', Henan: '河南', Hubei: '湖北', Hunan: '湖南', 'Inner Mongolia': '内蒙古',
    'Nei Mongol': '内蒙古', Jiangsu: '江苏', Jiangxi: '江西', Jilin: '吉林', Liaoning: '辽宁', Ningxia: '宁夏',
    'Ningxia Huizu': '宁夏', Qinghai: '青海', Shaanxi: '陕西', Shandong: '山东', Shanghai: '上海', Shanxi: '山西',
    Sichuan: '四川', Tianjin: '天津', Tibet: '西藏', Xizang: '西藏', Xinjiang: '新疆', 'Xinjiang Uygur': '新疆',
    Yunnan: '云南', Zhejiang: '浙江',
  },
  RU: {
    Moscow: '莫斯科', 'Sankt-Peterburg': '圣彼得堡', 'Saint Petersburg': '圣彼得堡',
    'Krasnodar Krai': '克拉斯诺达尔边疆区', "Tomskaya oblast'": '托木斯克州', Novosibirsk: '新西伯利亚',
  },
};

// Region names that are the country itself but differ from the browser's
// English country name.
const COUNTRY_ALIASES = new Set(['türkiye', 'turkey', 'the netherlands', 'czech republic', 'russia']);

let zhNames = null;
let enNames = null;
function displayName(names, code) {
  try {
    return names ? names.of(code) : '';
  } catch {
    return '';
  }
}
function names() {
  if (zhNames === null) {
    try {
      zhNames = new Intl.DisplayNames(['zh-Hans-CN', 'zh'], { type: 'region' });
      enNames = new Intl.DisplayNames(['en'], { type: 'region' });
    } catch {
      zhNames = false;
      enNames = false;
    }
  }
}

function parse(geo) {
  const s = (geo || '').trim();
  const m = s.match(/^(.*?),\s*([A-Za-z]{2})$/);
  if (!m) return { region: s, code: '' };
  return { region: m[1].trim(), code: m[2].toUpperCase() };
}

function countryZh(code) {
  names();
  return COUNTRY_ZH[code] || displayName(zhNames, code) || code;
}

// isCountry tells a region that only repeats the country's name.
function isCountry(region, code) {
  if (!region) return true;
  const r = region.toLowerCase();
  if (COUNTRY_ALIASES.has(r)) return true;
  names();
  const en = (displayName(enNames, code) || '').toLowerCase();
  return Boolean(en) && (en === r || en.includes(r) || r.includes(en));
}

// fmtGeo shows a vast location in the chosen mode; mode defaults to the
// saved one.
export function fmtGeo(geo, mode = geoMode()) {
  if (!geo) return '—';
  if (mode === 'raw') return geo;
  const { region, code } = parse(geo);
  if (!code) return geo;
  const country = countryZh(code);
  if (mode === 'continent') {
    const continent = CONTINENT_OF[code];
    return continent ? `${country}（${continent}）` : country;
  }
  if (isCountry(region, code)) return country;
  const local = (REGIONS_ZH[code] || {})[region];
  return `${country}·${local || region}`;
}
