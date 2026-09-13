const AGENT_COLORS = [
  '#7297bd', '#8f87b8', '#b08299', '#c47f7f',
  '#c08a66', '#c0a368', '#83b08a', '#6fa39b',
  '#76a8b8', '#7e95b5',
];

const AGENT_INTEGRATIONS = {
  ovad: [
    { id: 'github', name: 'GitHub' },
    { id: 'jira', name: 'Jira' },
    { id: 'confluence', name: 'Confluence' },
    { id: 'slack', name: 'Slack' },
    { id: 'azure', name: 'Azure' },
    { id: 'datadog', name: 'Datadog' },
    { id: 'aws', name: 'AWS' },
    { id: 'databricks', name: 'Databricks' },
    { id: 'clickhouse', name: 'ClickHouse' },
  ],
  goldsai: [
    { id: 'github', name: 'GitHub' },
    { id: 'nvd', name: 'NVD' },
    { id: 'slack', name: 'Slack' },
    { id: 'azure', name: 'Azure' },
  ],
  seihin: [
    { id: 'github', name: 'GitHub' },
    { id: 'jira', name: 'Jira' },
    { id: 'confluence', name: 'Confluence' },
    { id: 'slack', name: 'Slack' },
    { id: 'azure', name: 'Azure' },
    { id: 'datadog', name: 'Datadog' },
    { id: 'freshworks', name: 'Freshworks' },
  ],
  'agent-q': [
    { id: 'github', name: 'GitHub' },
    { id: 'jira', name: 'Jira' },
    { id: 'confluence', name: 'Confluence' },
    { id: 'slack', name: 'Slack' },
    { id: 'azure', name: 'Azure' },
  ],
  pulse: [
    { id: 'github', name: 'GitHub' },
    { id: 'jira', name: 'Jira' },
    { id: 'confluence', name: 'Confluence' },
    { id: 'slack', name: 'Slack' },
    { id: 'azure', name: 'Azure' },
    { id: 'salesforce', name: 'Salesforce' },
    { id: 'chorus', name: 'Chorus' },
    { id: 'datadog', name: 'Datadog' },
    { id: 'databricks', name: 'Databricks' },
    { id: 'freshworks', name: 'Freshworks' },
    { id: 'google', name: 'Google Drive / Sheets' },
  ],
};

const INTEGRATION_LOGOS = {
  slack: `<svg viewBox="0 0 128 128"><path d="M27.255 80.719c0 7.33-5.978 13.317-13.309 13.317C6.616 94.036.63 88.049.63 80.719s5.987-13.317 13.317-13.317h13.309zm6.709 0c0-7.33 5.987-13.317 13.317-13.317s13.317 5.986 13.317 13.317v33.335c0 7.33-5.986 13.317-13.317 13.317-7.33 0-13.317-5.987-13.317-13.317zm0 0" fill="#E01E5A"/><path d="M47.281 27.255c-7.33 0-13.317-5.978-13.317-13.309C33.964 6.616 39.951.63 47.281.63s13.317 5.987 13.317 13.317v13.309zm0 6.709c7.33 0 13.317 5.987 13.317 13.317s-5.986 13.317-13.317 13.317H13.946C6.616 60.598.63 54.612.63 47.281c0-7.33 5.987-13.317 13.317-13.317zm0 0" fill="#36C5F0"/><path d="M100.745 47.281c0-7.33 5.978-13.317 13.309-13.317 7.33 0 13.317 5.987 13.317 13.317s-5.987 13.317-13.317 13.317h-13.309zm-6.709 0c0 7.33-5.987 13.317-13.317 13.317S67.402 54.612 67.402 47.281V13.946C67.402 6.616 73.388.63 80.719.63c7.33 0 13.317 5.987 13.317 13.317zm0 0" fill="#2EB67D"/><path d="M80.719 100.745c7.33 0 13.317 5.978 13.317 13.309 0 7.33-5.987 13.317-13.317 13.317s-13.317-5.987-13.317-13.317v-13.309zm0-6.709c-7.33 0-13.317-5.987-13.317-13.317s5.986-13.317 13.317-13.317h33.335c7.33 0 13.317 5.986 13.317 13.317 0 7.33-5.987 13.317-13.317 13.317zm0 0" fill="#ECB22E"/></svg>`,
  github: `<svg viewBox="0 0 128 128"><g style="fill:var(--text-bright)"><path fill-rule="evenodd" clip-rule="evenodd" d="M64 5.103c-33.347 0-60.388 27.035-60.388 60.388 0 26.682 17.303 49.317 41.297 57.303 3.017.56 4.125-1.31 4.125-2.905 0-1.44-.056-6.197-.082-11.243-16.8 3.653-20.345-7.125-20.345-7.125-2.747-6.98-6.705-8.836-6.705-8.836-5.48-3.748.413-3.67.413-3.67 6.063.425 9.257 6.223 9.257 6.223 5.386 9.23 14.127 6.562 17.573 5.02.542-3.903 2.107-6.568 3.834-8.076-13.413-1.525-27.514-6.704-27.514-29.843 0-6.593 2.36-11.98 6.223-16.21-.628-1.52-2.695-7.662.584-15.98 0 0 5.07-1.623 16.61 6.19C53.7 35 58.867 34.327 64 34.304c5.13.023 10.3.694 15.127 2.033 11.526-7.813 16.59-6.19 16.59-6.19 3.287 8.317 1.22 14.46.593 15.98 3.872 4.23 6.215 9.617 6.215 16.21 0 23.194-14.127 28.3-27.574 29.796 2.167 1.874 4.097 5.55 4.097 11.183 0 8.08-.07 14.583-.07 16.572 0 1.607 1.088 3.49 4.148 2.897 23.98-7.994 41.263-30.622 41.263-57.294C124.388 32.14 97.35 5.104 64 5.104z"/><path d="M26.484 91.806c-.133.3-.605.39-1.035.185-.44-.196-.685-.605-.543-.906.13-.31.603-.395 1.04-.188.44.197.69.61.537.91zm2.446 2.729c-.287.267-.85.143-1.232-.28-.396-.42-.47-.983-.177-1.254.298-.266.844-.14 1.24.28.394.426.472.984.17 1.255zm2.382 3.477c-.37.258-.976.017-1.35-.52-.37-.538-.37-1.183.01-1.44.373-.258.97-.025 1.35.507.368.545.368 1.19-.01 1.452zm3.261 3.361c-.33.365-1.036.267-1.552-.23-.528-.487-.674-1.18-.343-1.544.336-.366 1.045-.264 1.564.23.527.486.686 1.18.33 1.544zm4.5 1.951c-.147.473-.825.688-1.51.486-.683-.207-1.13-.76-.99-1.238.14-.477.823-.7 1.512-.485.683.206 1.13.756.988 1.237zm4.943.361c.017.498-.563.91-1.28.92-.723.017-1.308-.387-1.315-.877 0-.503.568-.91 1.29-.924.717-.013 1.306.387 1.306.88zm4.598-.782c.086.485-.413.984-1.126 1.117-.7.13-1.35-.172-1.44-.653-.086-.498.422-.997 1.122-1.126.714-.123 1.354.17 1.444.663zm0 0"/></g></svg>`,
  jira: `<svg viewBox="0 0 128 128"><defs><linearGradient id="jira-a" x1="22.034" y1="9.773" x2="17.118" y2="14.842" gradientTransform="scale(4)" gradientUnits="userSpaceOnUse"><stop offset=".18" stop-color="#0052cc"/><stop offset="1" stop-color="#2684ff"/></linearGradient><linearGradient id="jira-b" x1="16.641" y1="15.63" x2="10.957" y2="21.09" gradientTransform="scale(4)" gradientUnits="userSpaceOnUse"><stop offset=".18" stop-color="#0052cc"/><stop offset="1" stop-color="#2684ff"/></linearGradient></defs><path d="M122.146 62.19L68.17 8.214 64 4.07 21.32 46.746 4.07 63.996l17.25 17.25L64 123.93l42.07-42.066.61-.61zm-58.15-16.31L80.62 62.5H46.56zm0 36.74L46.56 65.5h34.06z" fill="#2684ff"/><path d="M64 45.88C63.46 28.17 49.68 13.79 32.07 12.5L2.17 42.4l18.56 18.56z" fill="url(#jira-a)"/><path d="M81.1 62.5L64 79.62c.52 17.83 14.47 32.3 32.19 33.46l29.64-29.64-18.56-18.56z" fill="url(#jira-b)"/></svg>`,
  azure: `<svg viewBox="0 0 96 96"><path d="M33.34 6.54h26.04L32.35 86.66a4.15 4.15 0 0 1-3.94 2.81H8.15a4.15 4.15 0 0 1-3.93-5.49L29.4 9.35a4.15 4.15 0 0 1 3.94-2.81z" fill="#0078d4"/><path d="M71.17 60.26H29.88a1.91 1.91 0 0 0-1.31 3.31l26.53 24.76a4.18 4.18 0 0 0 2.85 1.13h23.38z" fill="#0078d4"/><path d="M66.6 9.35a4.15 4.15 0 0 0-3.94-2.81H33.65a4.15 4.15 0 0 1 3.94 2.81l25.18 74.63a4.15 4.15 0 0 1-3.93 5.49h29.01a4.15 4.15 0 0 0 3.93-5.49z" fill="#50e6ff"/></svg>`,
  nvd: `<svg viewBox="0 0 128 128"><rect width="128" height="128" rx="24" fill="#0032a0"/><path transform="translate(14 50.8) scale(0.1548)" fill="#fff" d="M45 0C20 0 0 20 0 45v125h40V45c0-4 5-6 8.5-3.5l104.9 115.1c29 29 77 8 77-31V-.4h-40v126c0 4-5 6-8.3 3.5L77.6 14.1c-10-10-18-14-32.5-14M250.2 0v125a45 45 0 0 0 45 45H463a52.5 52.5 0 0 0 0-105H357.8a12.5 12.5 0 0 1 0-25h177.5v130h40V40h70V0H357.8a52.5 52.5 0 0 0 0 105h105a12.5 12.5 0 0 1 0 25H295.3a5 5 0 0 1-5-5V0z"/></svg>`,
  salesforce: `<svg viewBox="0 0 128 128"><path fill="#00A1E0" d="M53.01 31.44c3.98-4.14 9.51-6.71 15.64-6.71 8.14 0 15.24 4.54 19.02 11.28a26.26 26.26 0 0110.75-2.29c14.68 0 26.58 12.01 26.58 26.81 0 14.81-11.9 26.82-26.58 26.82-1.79 0-3.54-.18-5.24-.52-3.33 5.94-9.68 9.95-16.96 9.95-3.05 0-5.93-.7-8.5-1.96-3.38 7.94-11.24 13.51-20.41 13.51-9.55 0-17.68-6.04-20.8-14.51-1.36.29-2.78.44-4.23.44-11.37 0-20.58-9.31-20.58-20.79 0-7.7 4.14-14.42 10.29-18.01a23.727 23.727 0 01-1.97-9.51c0-13.21 10.72-23.92 23.95-23.92 7.76-.01 14.67 3.69 19.04 9.41"/></svg>`,
  chorus: `<svg viewBox="0 0 128 128"><circle cx="64" cy="64" r="56" fill="#4B0082"/><path d="M40 48c0-4.4 3.6-8 8-8s8 3.6 8 8v32c0 4.4-3.6 8-8 8s-8-3.6-8-8V48zm32 0c0-4.4 3.6-8 8-8s8 3.6 8 8v32c0 4.4-3.6 8-8 8s-8-3.6-8-8V48zM56 38c0-4.4 3.6-8 8-8s8 3.6 8 8v52c0 4.4-3.6 8-8 8s-8-3.6-8-8V38z" fill="#fff"/></svg>`,
  confluence: `<svg viewBox="0 0 128 128"><path d="M9.26 101.37c-1.65 2.7-3.63 5.86-4.87 7.75a3.28 3.28 0 0 0 1.07 4.5l22.56 13.74a3.28 3.28 0 0 0 4.5-1.07c1.07-1.81 2.95-4.88 5-8.33 9.19-15.07 18.47-13.25 35.43-5.42l22.23 10.23a3.28 3.28 0 0 0 4.33-1.65l11.16-24.64a3.28 3.28 0 0 0-1.65-4.33C99.4 88.04 57.06 68.63 9.26 101.37z" fill="#2684FF"/><path d="M118.74 26.63c1.65-2.7 3.63-5.86 4.87-7.75a3.28 3.28 0 0 0-1.07-4.5L100.04.64a3.28 3.28 0 0 0-4.5 1.07c-1.07 1.81-2.95 4.88-5 8.33-9.19 15.07-18.47 13.25-35.43 5.42L33.13 5.31a3.28 3.28 0 0 0-4.33 1.65L17.64 31.6a3.28 3.28 0 0 0 1.65 4.33c9.11 4.17 51.45 23.49 99.45-9.3z" fill="#2684FF"/></svg>`,
  datadog: `<svg viewBox="0 0 24 24"><path d="M12.2596 1.3882L14.7979 3.3402l.4651-2.4531 3.1608.826-1.0881 2.5895 2.604.857L16.952 19.023l-3.3896-2.1778L11.382 19.572l-1.3761-1.5752L4.77 21.067l2.762-11.097-2.5697-1.808 2.4424-2.0697-.9497-3.03L10.098.4427l.9768 2.7024z" fill="#632CA6"/><path d="M8.6766 10.7313l-1.6695 7.86 3.5949-2.1073.9927 1.1365 1.808-2.2622 3.0498 1.958 2.0621-9.5839-2.2553-.7424-.4018.3977-1.9753-1.5198-1.4071 1.775-2.03-.625-.5758 1.3574-.1723 1.5047.589.5225-.7474.965z" fill="#fff"/></svg>`,
  databricks: `<svg viewBox="0 0 128 128"><rect width="128" height="128" rx="24" fill="#FF3621"/><g fill="#fff"><path d="M64 24 106 42 64 60 22 42z" opacity=".9"/><path d="M22 58 64 76 106 58 106 66 64 84 22 66z"/><path d="M22 78 64 96 106 78 106 86 64 104 22 86z"/></g></svg>`,
  clickhouse: `<svg viewBox="0 0 128 128"><rect width="128" height="128" rx="24" fill="#FFCC00"/><g fill="#21232B"><rect x="26" y="30" width="15" height="68"/><rect x="49" y="30" width="15" height="68"/><rect x="72" y="30" width="15" height="68"/><rect x="95" y="30" width="15" height="30"/></g></svg>`,
  freshworks: `<svg viewBox="0 0 128 128"><rect width="128" height="128" rx="24" fill="#25C16F"/><path fill="#fff" d="M74 34H54c-11 0-20 9-20 20v40h18V54c0-1.1.9-2 2-2h20zM74 62H60v18c0 8.8 7.2 16 16 16h18V80H76c-1.1 0-2-.9-2-2z"/></svg>`,
  google: `<svg viewBox="0 0 87.3 78"><path fill="#0066da" d="m6.6 66.85 3.85 6.65c.8 1.4 1.95 2.5 3.3 3.3l13.75-23.8h-27.5c0 1.55.4 3.1 1.2 4.5z"/><path fill="#00ac47" d="m43.65 25-13.75-23.8c-1.35.8-2.5 1.9-3.3 3.3l-25.4 44a9.06 9.06 0 0 0-1.2 4.5h27.5z"/><path fill="#ea4335" d="m73.55 76.8c1.35-.8 2.5-1.9 3.3-3.3l1.6-2.75 7.65-13.25c.8-1.4 1.2-2.95 1.2-4.5h-27.502l5.852 11.5z"/><path fill="#00832d" d="m43.65 25 13.75-23.8c-1.35-.8-2.9-1.2-4.5-1.2h-18.5c-1.6 0-3.15.45-4.5 1.2z"/><path fill="#2684fc" d="m59.8 53h-32.3l-13.75 23.8c1.35.8 2.9 1.2 4.5 1.2h50.8c1.6 0 3.15-.45 4.5-1.2z"/><path fill="#ffba00" d="m73.4 26.5-12.7-22c-.8-1.4-1.95-2.5-3.3-3.3l-13.75 23.8 16.15 28h27.45c0-1.55-.4-3.1-1.2-4.5z"/></svg>`,
  aws: `<svg viewBox="0 0 128 128"><path style="fill:var(--text-bright)" d="M36.4 54.4c0 1.6.2 2.9.5 3.8.4.9.9 1.9 1.6 3 .2.4.3.8.3 1.1 0 .5-.3 1-.9 1.4l-3 2c-.4.3-.9.4-1.3.4-.5 0-1-.2-1.5-.7-.7-.7-1.3-1.5-1.8-2.4-.5-.9-1-1.9-1.5-3.1-3.7 4.4-8.4 6.5-14 6.5-4 0-7.2-1.2-9.5-3.5C2.8 60.7 1.7 57.7 1.7 54c0-4 1.4-7.2 4.2-9.6 2.9-2.4 6.7-3.6 11.5-3.6 1.6 0 3.3.1 5 .4 1.7.2 3.5.6 5.4 1v-3.4c0-3.6-.8-6.1-2.2-7.6-1.5-1.5-4.1-2.2-7.7-2.2-1.7 0-3.4.2-5.1.6-1.7.4-3.4.9-5 1.6-.7.3-1.2.5-1.6.6-.3.1-.6.1-.8.1-.7 0-1-.5-1-1.5v-2.4c0-.8.1-1.4.3-1.7.2-.3.6-.6 1.2-.9 1.7-.9 3.7-1.6 6-2.2 2.4-.6 4.9-.9 7.5-.9 5.8 0 10 1.3 12.7 3.9 2.7 2.6 4 6.6 4 11.9zM20.1 60.5c1.6 0 3.2-.3 5-.9 1.7-.6 3.3-1.6 4.5-3.1.8-.9 1.3-1.9 1.6-3 .3-1.1.4-2.5.4-4.1v-2c-1.3-.3-2.8-.6-4.3-.8-1.5-.2-3-.3-4.5-.3-3.2 0-5.5.6-7.1 1.9-1.6 1.3-2.4 3.1-2.4 5.5 0 2.3.6 4 1.8 5.1 1.2 1.2 2.9 1.7 5 1.7zm32.2 4.3c-.8 0-1.3-.1-1.7-.4-.4-.3-.7-.9-1-1.6L40.1 30c-.3-.9-.5-1.5-.5-1.9 0-.8.4-1.2 1.2-1.2h4.6c.9 0 1.5.1 1.8.4.4.3.7.9.9 1.6l6.8 26.8 6.3-26.8c.2-.9.5-1.4.9-1.6.4-.2 1-.4 1.9-.4h3.7c.9 0 1.5.1 1.9.4.4.3.7.9.9 1.6l6.4 27.2 7-27.2c.2-.9.5-1.4.9-1.6.4-.2 1-.4 1.8-.4h4.3c.8 0 1.2.4 1.2 1.2 0 .2 0 .5-.1.8-.1.3-.2.7-.4 1.2L83.6 62.9c-.3.9-.6 1.4-1 1.6-.4.2-.9.4-1.7.4h-4c-.9 0-1.5-.1-1.9-.4-.4-.3-.7-.9-.9-1.6l-6.3-26.1-6.2 26c-.2.9-.5 1.4-.9 1.6-.4.3-1 .4-1.9.4zm51.5 1.3c-2.5 0-5-.3-7.4-.9-2.4-.6-4.3-1.2-5.5-1.9-.8-.4-1.3-.9-1.5-1.3-.2-.4-.3-.9-.3-1.3v-2.5c0-1 .4-1.5 1.1-1.5.3 0 .6.1.9.2.3.1.8.3 1.3.5 1.7.7 3.5 1.3 5.5 1.7 2 .4 3.9.6 5.8.6 3.1 0 5.5-.5 7.1-1.6 1.7-1.1 2.5-2.7 2.5-4.7 0-1.4-.4-2.5-1.3-3.4-.9-.9-2.5-1.8-4.9-2.6l-7-2.2c-3.5-1.1-6.1-2.7-7.7-4.9-1.6-2.1-2.4-4.4-2.4-6.9 0-2 .4-3.8 1.3-5.3.9-1.5 2.1-2.9 3.6-3.9 1.5-1.1 3.2-1.9 5.2-2.5 2-.6 4.1-.8 6.2-.8 1.1 0 2.2.1 3.3.2 1.1.1 2.1.3 3.1.5 1 .2 1.9.5 2.8.7.9.3 1.6.5 2.1.8.7.4 1.2.8 1.5 1.2.3.4.4.9.4 1.5v2.3c0 1-.4 1.5-1.1 1.5-.4 0-1-.2-1.8-.5-2.7-1.2-5.7-1.8-9.1-1.8-2.8 0-5 .5-6.5 1.4-1.5.9-2.3 2.4-2.3 4.4 0 1.4.5 2.6 1.5 3.5 1 .9 2.8 1.8 5.4 2.7l6.9 2.2c3.5 1.1 6 2.6 7.5 4.6 1.5 2 2.3 4.2 2.3 6.7 0 2.1-.4 4-1.3 5.7-.9 1.7-2.1 3.2-3.6 4.4-1.5 1.2-3.3 2-5.4 2.7-2.2.7-4.4 1.1-6.8 1.1z"/><path fill="#F90" d="M115.9 97c-14 10.3-34.3 15.8-51.8 15.8-24.5 0-46.6-9.1-63.3-24.2-1.3-1.2-.1-2.8 1.5-1.9 18 10.5 40.3 16.8 63.3 16.8 15.5 0 32.6-3.2 48.3-9.9 2.3-1 4.3 1.5 2 3.4z"/><path fill="#F90" d="M121.8 90.3c-1.8-2.3-11.8-1.1-16.3-.5-1.4.2-1.6-1-.3-1.9 8-5.6 21.1-4 22.6-2.1 1.5 1.9-.4 15-7.9 21.3-1.2.9-2.3.5-1.8-.8 1.6-4.3 5.5-14 3.7-16z"/></svg>`,
};

const SOURCE_LABELS = { slack: 'Slack commands', chat: 'Web chat', workflow: 'Scheduled workflows', dashboard: 'Dashboard renders' };
const SLACK_ID_RE = /^[UW][A-Z0-9]{6,}$/;
const PAGES = ['overview', 'integrations', 'mcp', 'agents', 'chats', 'skills', 'workflows', 'dashboards', 'changelog', 'billing'];
const PAGE_TITLES = {
  overview: 'Overview', integrations: 'Integrations', mcp: 'MCP & Connectors', agents: 'Agents', chats: 'Chats',
  skills: 'Skills', workflows: 'Workflows', dashboards: 'Dashboards', changelog: 'Changelog', billing: 'Usage & Billing',
};
const WINDOWS = [7, 30, 90, 0];
const EXTRAS_CACHE_TTL_MS = 5 * 60 * 1000;
const REFRESH_MS = 30000;

let appTitle = 'arbetern';
let currentPage = null;
let pageBeforeChat = 'agents';
let days = 30;
try { const w = parseInt(localStorage.getItem('arbetern-window'), 10); if (WINDOWS.includes(w)) days = w; } catch (_) {}

let agentsData = [];
let integrationsData = null;
let workflowsData = null;
let dashboardsData = null;
let changesState = { list: null, error: null };
let sessionsData = null;
let sessionsFetched = false;
let billingSummary = null;
let billingSummaryDays = null;
const gitops = { workflows: null, dashboards: null };
const lastFetched = {};
const inflight = new Set();
const AGENT_DASHBOARDS = {};
const AGENT_WORKFLOWS = {};
let wfFilter = 'all';
let dashFilter = 'all';
let chatsAgent = null;
let chatsList = null;
let chatsLoading = false;
let skillsData = null;
let skillFilter = 'all';
let mcpData = null;

function escapeHtml(str) {
  return String(str == null ? '' : str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function safeExternalUrl(value) {
  try {
    const url = new URL(String(value || ''), window.location.origin);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : '';
  } catch (e) {
    return '';
  }
}

function safeImageUrl(value) {
  const url = safeExternalUrl(value);
  return url && url.startsWith('https://') ? url : '';
}

// Agent and descriptor ids are validated server-side against this alphabet;
// anything else is never interpolated into markup.
const SAFE_ID_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
function safeId(id) {
  return typeof id === 'string' && SAFE_ID_RE.test(id) ? id : null;
}

function elem(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

function httpError(status) {
  const e = new Error('HTTP ' + status);
  e.status = status;
  return e;
}

async function fetchJSON(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw httpError(r.status);
  return r.json();
}

function recentlyFetched(key, ms = 4000) {
  const now = Date.now();
  if (lastFetched[key] && now - lastFetched[key] < ms) return true;
  lastFetched[key] = now;
  return false;
}

function timeAgo(iso) {
  const t = new Date(iso).getTime();
  if (!t || t < 0) return '';
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  if (s < 2592000) return Math.floor(s / 86400) + 'd ago';
  return new Date(t).toLocaleDateString();
}

const fmtInt = n => (n || 0).toLocaleString('en-US');
const money = n => '$' + (n || 0).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
const money4 = n => (n >= 1 ? money(n) : '$' + (n || 0).toFixed(4));
function tok(n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return '' + n;
}
const plural = (n, word) => `${fmtInt(n)} ${word}${n === 1 ? '' : 's'}`;
const windowLabel = () => (days ? `last ${days} days` : 'all time');
const emptyHtml = (text, tall) => `<div class="empty${tall ? ' tall' : ''}">${escapeHtml(text)}</div>`;

function hashColor(str) {
  let hash = 0;
  for (let i = 0; i < str.length; i++) hash = str.charCodeAt(i) + ((hash << 5) - hash);
  return AGENT_COLORS[Math.abs(hash) % AGENT_COLORS.length];
}

function agentById(id) { return agentsData.find(a => a.id === id); }
function agentLabel(id) { const a = agentById(id); return a ? a.name : (id || '(unknown)'); }
function agentColorFor(id) { return hashColor(agentLabel(id)); }

function miniAvatar(id) {
  const name = agentLabel(id);
  return `<span class="mini-avatar" style="background:${agentColorFor(id)}" title="${escapeHtml(name)}">${escapeHtml(name.charAt(0).toUpperCase())}</span>`;
}

function agentChip(id) {
  return `<span class="agent-chip">${miniAvatar(id)}${escapeHtml(agentLabel(id))}</span>`;
}

function getIntegrationLogo(id) { return INTEGRATION_LOGOS[id] || ''; }

function getAgentProfession(agent) {
  const general = (agent.prompts || {}).general || '';
  const match = general.match(/You are (?:a |an )?([^.\n]+)/i);
  return match ? match[1].trim() : agent.id;
}

/* Shell: theme, side rail, routing */
(function initTheme() {
  const btn = document.getElementById('theme-toggle');
  function label(t) {
    btn.title = 'Switch to ' + (t === 'light' ? 'dark' : 'light') + ' theme';
    btn.setAttribute('aria-checked', String(t !== 'light'));
  }
  btn.addEventListener('click', () => {
    const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('arbetern-theme', next); } catch (e) {}
    label(next);
  });
  label(document.documentElement.dataset.theme || 'dark');
})();

function setSidebarCollapsed(collapsed) {
  const html = document.documentElement;
  if (collapsed) html.dataset.sidebar = 'collapsed'; else delete html.dataset.sidebar;
  const btn = document.getElementById('sidebar-collapse');
  btn.title = collapsed ? 'Expand navigation' : 'Collapse navigation';
  btn.setAttribute('aria-label', btn.title);
  btn.setAttribute('aria-expanded', String(!collapsed));
  try { localStorage.setItem('arbetern-sidebar', collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
}

function openDrawer() { document.documentElement.dataset.drawer = 'open'; }
function closeDrawer() { delete document.documentElement.dataset.drawer; }

document.getElementById('sidebar-collapse').addEventListener('click', () => {
  setSidebarCollapsed(document.documentElement.dataset.sidebar !== 'collapsed');
});
setSidebarCollapsed(document.documentElement.dataset.sidebar === 'collapsed');
document.getElementById('menu-toggle').addEventListener('click', () => {
  if (document.documentElement.dataset.drawer === 'open') closeDrawer(); else openDrawer();
});
document.getElementById('sidebar-scrim').addEventListener('click', closeDrawer);

document.addEventListener('click', e => {
  const a = e.target.closest('a[data-page]');
  if (!a || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
  e.preventDefault();
  navigate(a.dataset.page);
});

function pathForPage(page) { return page === 'overview' ? '/ui/' : '/ui/' + page; }

function pageFromPath(path) {
  const m = path.match(/^\/ui\/([a-z-]+)\/?$/);
  return m && PAGES.includes(m[1]) ? m[1] : 'overview';
}

function showPage(page) {
  const changed = page !== currentPage;
  currentPage = page;
  document.querySelectorAll('.page').forEach(p => { p.hidden = p.dataset.page !== page; });
  document.querySelectorAll('.nav-item').forEach(a => {
    const active = a.dataset.page === page;
    a.classList.toggle('active', active);
    if (active) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  document.title = `${appTitle} — ${PAGE_TITLES[page]}`;
  if (changed) window.scrollTo(0, 0);
  loadPage(page);
}

function navigate(page, { replace = false } = {}) {
  const path = pathForPage(page);
  if (location.pathname !== path) history[replace ? 'replaceState' : 'pushState']({}, '', path);
  closeDrawer();
  if (chatFull) hideFullChat();
  showPage(page);
}

function applyRoute() {
  const m = location.pathname.match(/^\/ui\/([^/]+)\/chat\/?$/);
  if (m) {
    const id = decodeURIComponent(m[1]);
    const agent = agentById(id);
    if (agent && agent.chat_enabled) {
      if (!currentPage) showPage('agents');
      openFullChat(id);
      return;
    }
    if (!agentsData.length) { if (!currentPage) showPage('agents'); return; }
  }
  if (chatFull) hideFullChat();
  showPage(pageFromPath(location.pathname));
}

window.addEventListener('popstate', applyRoute);

// Every view is painted whenever data lands, hidden pages included, so opening
// a rail item shows finished content instead of a placeholder.
function renderViews() {
  renderOverview();
  if (integrationsData) renderIntegrations(integrationsData);
  renderWorkflowsPage();
  renderDashboardsPage();
  renderSkillsPage();
  renderMCPPage();
  renderChatsPage();
  renderChanges();
  if (billingSummary) renderBilling(billingSummary);
}

function loadPage(page) {
  switch (page) {
    case 'overview':
      renderOverview();
      return Promise.all([loadBilling(), loadSessions(), loadIntegrations(), loadWorkflows(), loadDashboards(), loadChanges(), loadMCP()]);
    case 'integrations':
      if (integrationsData) renderIntegrations(integrationsData);
      return loadIntegrations();
    case 'agents':
      return Promise.all([loadDashboards(), loadWorkflows()]);
    case 'workflows':
      renderWorkflowsPage();
      return Promise.all([loadWorkflows(), loadGitops('workflows')]);
    case 'dashboards':
      renderDashboardsPage();
      return Promise.all([loadDashboards(), loadGitops('dashboards')]);
    case 'changelog':
      renderChanges();
      return loadChanges();
    case 'billing':
      if (billingSummary) renderBilling(billingSummary);
      return loadBilling();
    case 'mcp':
      renderMCPPage();
      return loadMCP();
    case 'chats':
      renderChatsPage();
      return loadChats();
    case 'skills':
      renderSkillsPage();
      return loadSkills();
    default:
      return Promise.resolve();
  }
}

/* Time window shared by Overview and Usage & Billing */
function syncPills() {
  document.querySelectorAll('[data-pills="window"] .pill').forEach(b => b.classList.toggle('active', +b.dataset.days === days));
}
document.querySelectorAll('[data-pills="window"]').forEach(group => {
  group.addEventListener('click', e => {
    const b = e.target.closest('.pill');
    if (!b) return;
    days = +b.dataset.days;
    try { localStorage.setItem('arbetern-window', String(days)); } catch (_) {}
    syncPills();
    delete lastFetched['billing:' + days];
    loadPage(currentPage);
  });
});
syncPills();

/* Loaders */
async function loadAgents() {
  try {
    agentsData = await fetchJSON('/api/agents');
    renderAgents(agentsData);
    applyRoute();
    if (hydrateExtrasFromCache()) applyAgentExtras();
    renderViews();
  } catch (err) {
    console.error('Failed to load agents:', err);
    document.getElementById('agents-grid').innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">&#x26a0;&#xfe0f;</div>
        <p>Failed to load agents. Is the server running?</p>
      </div>`;
  }
}

async function loadIntegrations() {
  if (recentlyFetched('integrations')) return;
  try {
    integrationsData = await fetchJSON('/api/integrations');
    renderViews();
  } catch (err) {
    console.error('Failed to load integrations:', err);
    document.getElementById('integrations-grid').innerHTML = `
      <div class="empty-state" style="grid-column:1/-1;padding:30px;"><p>Failed to load integrations.</p></div>`;
  }
}

async function loadWorkflows() {
  if (recentlyFetched('workflows')) return;
  try {
    const list = await fetchJSON('/api/workflows');
    populateWorkflows(list);
    writeExtrasCache('extras.workflows', list);
    applyAgentExtras();
    renderViews();
  } catch (err) {
    console.warn('Failed to load workflows:', err);
  }
}

async function loadDashboards() {
  if (recentlyFetched('dashboards')) return;
  try {
    const list = await fetchJSON('/api/dashboards');
    populateDashboards(list);
    writeExtrasCache('extras.dashboards', list);
    applyAgentExtras();
    renderViews();
  } catch (err) {
    console.warn('Failed to load dashboards:', err);
  }
}

async function loadChanges() {
  if (recentlyFetched('changes')) return;
  try {
    changesState = { list: await fetchJSON('/api/changes'), error: null };
  } catch (err) {
    console.error('Failed to load changes:', err);
    changesState = { list: null, error: err };
  }
  renderChanges();
  renderLatestChange();
}

async function loadSessions() {
  if (recentlyFetched('sessions')) return;
  try {
    sessionsData = await fetchJSON('/api/sessions');
  } catch (err) {
    sessionsData = null;
  }
  sessionsFetched = true;
  renderSessions();
}

async function loadBilling() {
  const windowDays = days;
  const key = 'billing:' + windowDays;
  if (inflight.has(key)) return;
  const fresh = recentlyFetched(key);
  if (fresh && billingSummaryDays === windowDays) return;
  inflight.add(key);
  try {
    const sum = await fetchJSON('/api/billing/summary?days=' + windowDays);
    if (windowDays !== days) return;
    billingSummary = sum;
    billingSummaryDays = windowDays;
    renderViews();
  } catch (err) {
    console.warn('Failed to load usage summary:', err);
  } finally {
    inflight.delete(key);
  }
}

async function loadGitops(kind, force) {
  if (!force && recentlyFetched('gitops:' + kind)) return;
  try {
    gitops[kind] = await fetchJSON(`/api/${kind}/_gitops`);
  } catch (err) {
    gitops[kind] = null;
  }
  renderViews();
}

function readExtrasCache(key) {
  try {
    const raw = sessionStorage.getItem(key);
    if (!raw) return null;
    const { t, v } = JSON.parse(raw);
    if (!t || (Date.now() - t) > EXTRAS_CACHE_TTL_MS) return null;
    return v;
  } catch (_) { return null; }
}

function writeExtrasCache(key, value) {
  try { sessionStorage.setItem(key, JSON.stringify({ t: Date.now(), v: value })); } catch (_) {}
}

function hydrateExtrasFromCache() {
  let hydrated = false;
  const d = readExtrasCache('extras.dashboards');
  if (d) { populateDashboards(d); hydrated = true; }
  const w = readExtrasCache('extras.workflows');
  if (w) { populateWorkflows(w); hydrated = true; }
  return hydrated;
}

function populateDashboards(list) {
  dashboardsData = list || [];
  for (const k of Object.keys(AGENT_DASHBOARDS)) delete AGENT_DASHBOARDS[k];
  dashboardsData.forEach(d => {
    if (d.template_id) return;
    if (!AGENT_DASHBOARDS[d.agent]) AGENT_DASHBOARDS[d.agent] = [];
    AGENT_DASHBOARDS[d.agent].push(d);
  });
}

function populateWorkflows(list) {
  workflowsData = list || [];
  for (const k of Object.keys(AGENT_WORKFLOWS)) delete AGENT_WORKFLOWS[k];
  workflowsData.forEach(w => {
    if (!AGENT_WORKFLOWS[w.agent]) AGENT_WORKFLOWS[w.agent] = [];
    AGENT_WORKFLOWS[w.agent].push(w);
  });
}

/* Overview */
function renderOverview() {
  if (billingSummary) {
    renderMatrix(billingSummary);
    renderRequests(billingSummary);
    renderAgentsBoard(billingSummary);
    renderTopUsers(billingSummary);
    renderSources(billingSummary);
    renderRecent(billingSummary);
  }
  renderSessions();
  renderFleet();
  renderLatestChange();
}

function userLabel(row) { return row.name || row.key; }

function userNamesFrom(sum) {
  const names = new Map();
  (sum.by_user || []).forEach(r => { if (r.name) names.set(r.key, r.name); });
  return names;
}

function renderMatrix(sum) {
  const el = document.getElementById('matrix');
  const meta = document.getElementById('matrix-meta');
  const users = new Map();
  const agentTotals = new Map();
  (sum.by_user_agent || []).forEach(r => {
    const n = r.counts.requests;
    if (!n) return;
    let u = users.get(r.key);
    if (!u) { u = { key: r.key, name: r.name || '', total: 0, cells: {} }; users.set(r.key, u); }
    if (r.name && !u.name) u.name = r.name;
    u.total += n;
    u.cells[r.agent] = (u.cells[r.agent] || 0) + n;
    agentTotals.set(r.agent, (agentTotals.get(r.agent) || 0) + n);
  });
  if (!users.size) {
    el.innerHTML = emptyHtml('No attributed activity in this window yet. Slack commands and chat turns land here as people use the agents.', true);
    meta.textContent = '';
    return;
  }
  const agents = [...agentTotals.entries()].sort((a, b) => b[1] - a[1]).map(e => e[0]);
  const sorted = [...users.values()].sort((a, b) => b.total - a.total);
  const shown = sorted.slice(0, 12);
  let max = 1;
  shown.forEach(u => agents.forEach(a => { max = Math.max(max, u.cells[a] || 0); }));
  const alpha = v => (v ? +(0.12 + 0.78 * Math.sqrt(v / max)).toFixed(2) : 0);

  const head = `<tr><th class="mx-corner"></th>${agents.map(a =>
    `<th class="mx-agent-head">${miniAvatar(a)}<small>${escapeHtml(agentLabel(a))}</small></th>`).join('')}<th class="mx-total-head">Total</th></tr>`;

  const body = shown.map(u => {
    const cells = agents.map(a => {
      const v = u.cells[a] || 0;
      const al = alpha(v);
      const cls = ['mx-cell', v ? '' : 'zero', al >= 0.6 ? 'hi' : ''].filter(Boolean).join(' ');
      const title = `${userLabel(u)} · ${agentLabel(a)} · ${plural(v, 'request')}`;
      return `<td class="${cls}" style="--a:${al}" title="${escapeHtml(title)}">${v ? fmtInt(v) : ''}</td>`;
    }).join('');
    return `<tr>${userCellHtml(u)}${cells}<td class="mx-total">${fmtInt(u.total)}</td></tr>`;
  }).join('');

  const grand = [...agentTotals.values()].reduce((s, v) => s + v, 0);
  const foot = `<tr><td class="mx-user">Everyone</td>${agents.map(a =>
    `<td class="mx-cell">${fmtInt(agentTotals.get(a))}</td>`).join('')}<td class="mx-total">${fmtInt(grand)}</td></tr>`;

  const hidden = sorted.length - shown.length;
  const legend = `<div class="matrix-legend">
      <span><span class="scale">${[0.15, 0.35, 0.6, 0.9].map(a => `<i style="--a:${a}"></i>`).join('')}</span>more requests</span>
      ${hidden > 0 ? `<span>${plural(hidden, 'more user')} not shown</span>` : ''}
    </div>`;

  el.innerHTML = `<table class="matrix"><thead>${head}</thead><tbody>${body}</tbody><tfoot>${foot}</tfoot></table>${legend}`;
  meta.textContent = `${plural(sorted.length, 'user')} · ${plural(agents.length, 'agent')} · ${windowLabel()}`;
}

function userCellHtml(u) {
  if (u.name) {
    return `<td class="mx-user" title="${escapeHtml(u.key)}">${escapeHtml(u.name)}<small>${escapeHtml(u.key)}</small></td>`;
  }
  const raw = SLACK_ID_RE.test(u.key);
  return `<td class="mx-user${raw ? ' raw' : ''}" title="${escapeHtml(u.key)}">${escapeHtml(u.key)}</td>`;
}

function fillDaily(daily, windowDays) {
  const byKey = new Map((daily || []).map(d => [d.key, d]));
  if (!windowDays) return [...byKey.values()].sort((a, b) => (a.key < b.key ? -1 : 1));
  const out = [];
  const end = new Date();
  end.setUTCHours(0, 0, 0, 0);
  for (let i = windowDays - 1; i >= 0; i--) {
    const d = new Date(end.getTime() - i * 86400000);
    const key = d.toISOString().slice(0, 10);
    out.push(byKey.get(key) || { key, counts: { requests: 0, cost_usd: 0, total_tokens: 0 } });
  }
  return out;
}

function renderBars(el, rows, valueOf, labelOf, fromEl, toEl) {
  if (!rows || !rows.length) {
    el.innerHTML = '<div class="empty">No daily data</div>';
    fromEl.textContent = '—';
    toEl.textContent = '—';
    return;
  }
  const max = Math.max(...rows.map(valueOf), 0.0001);
  el.innerHTML = rows.map(d => {
    const v = valueOf(d);
    return `<div class="bar${v ? '' : ' empty-day'}" style="height:${Math.max(2, v / max * 100)}%"><span>${escapeHtml(d.key)}: ${labelOf(v)}</span></div>`;
  }).join('');
  fromEl.textContent = rows[0].key;
  toEl.textContent = rows[rows.length - 1].key;
}

function renderRequests(sum) {
  const t = sum.totals || {};
  document.getElementById('ov-req').textContent = fmtInt(t.requests);
  document.getElementById('ov-req-sub').textContent = t.requests
    ? `${tok(t.total_tokens)} tokens · ${money(t.cost_usd)} estimated`
    : 'No turns recorded in this window';
  document.getElementById('req-meta').textContent = windowLabel();
  renderBars(document.getElementById('ov-bars'), fillDaily(sum.daily, days), d => d.counts.requests || 0,
    v => plural(v, 'request'), document.getElementById('ov-axis-from'), document.getElementById('ov-axis-to'));
}

function renderAgentsBoard(sum) {
  const el = document.getElementById('ov-agents');
  const by = new Map((sum.by_agent || []).map(r => [r.key, r.counts]));
  const usersPer = new Map();
  (sum.by_user_agent || []).forEach(r => {
    if (!r.counts.requests) return;
    if (!usersPer.has(r.agent)) usersPer.set(r.agent, new Set());
    usersPer.get(r.agent).add(r.key);
  });
  const ids = new Set(agentsData.map(a => a.id));
  by.forEach((_, id) => ids.add(id));
  const rows = [...ids].map(id => ({ id, c: by.get(id) || { requests: 0, total_tokens: 0, cost_usd: 0 } }))
    .sort((a, b) => b.c.requests - a.c.requests || agentLabel(a.id).localeCompare(agentLabel(b.id)));
  if (!rows.length) { el.innerHTML = emptyHtml('No agents discovered.'); return; }
  const max = Math.max(...rows.map(r => r.c.requests), 1);
  el.innerHTML = rows.map(r => {
    const req = r.c.requests;
    const people = usersPer.get(r.id) ? usersPer.get(r.id).size : 0;
    const sub = req
      ? `${plural(people, 'user')} · ${tok(r.c.total_tokens)} tokens · ${money(r.c.cost_usd)}`
      : 'No requests in this window';
    return `<div class="lb-row${req ? '' : ' idle'}">
        ${miniAvatar(r.id)}
        <div><div class="lb-name">${escapeHtml(agentLabel(r.id))}</div><div class="lb-sub">${sub}</div></div>
        <div class="lb-bar"><i style="--w:${(req / max * 100).toFixed(1)}%;--fill:${agentColorFor(r.id)}"></i></div>
        <div class="lb-num">${fmtInt(req)}</div>
      </div>`;
  }).join('');
  const active = rows.filter(r => r.c.requests).length;
  document.getElementById('ov-agents-meta').textContent = `${active} of ${rows.length} active`;
}

function renderTopUsers(sum) {
  const el = document.getElementById('ov-users');
  const rows = (sum.by_user || []).filter(r => r.counts.requests > 0)
    .sort((a, b) => b.counts.requests - a.counts.requests);
  document.getElementById('ov-users-meta').textContent = rows.length ? plural(rows.length, 'user') : '';
  if (!rows.length) { el.innerHTML = emptyHtml('No user-attributed turns yet.'); return; }
  el.innerHTML = rows.slice(0, 8).map((r, i) => `<div class="ur-row">
      <span class="ur-rank">${i + 1}</span>
      <div><div class="lb-name" title="${escapeHtml(r.key)}">${escapeHtml(userLabel(r))}</div>
        <div class="lb-sub">${tok(r.counts.total_tokens)} tokens · ${money4(r.counts.cost_usd)}</div></div>
      <span class="lb-num">${fmtInt(r.counts.requests)}</span>
    </div>`).join('');
}

function renderSources(sum) {
  const el = document.getElementById('ov-sources');
  const rows = (sum.by_source || []).filter(r => r.counts.requests > 0)
    .sort((a, b) => b.counts.requests - a.counts.requests);
  const total = rows.reduce((s, r) => s + r.counts.requests, 0);
  if (!total) { el.innerHTML = emptyHtml('No requests in this window.'); return; }
  const cls = k => (SOURCE_LABELS[k] ? k : 'other');
  const seg = rows.map(r => `<i class="src-${cls(r.key)}" style="width:${(r.counts.requests / total * 100).toFixed(2)}%" title="${escapeHtml(r.key)}: ${fmtInt(r.counts.requests)}"></i>`).join('');
  const legend = rows.map(r => `<span><span class="dot src-${cls(r.key)}"></span>${escapeHtml(SOURCE_LABELS[r.key] || r.key)}</span>
      <span class="n">${fmtInt(r.counts.requests)}</span><span class="pct">${Math.round(r.counts.requests / total * 100)}%</span>`).join('');
  el.innerHTML = `<div class="seg">${seg}</div><div class="seg-legend">${legend}</div>`;
}

function renderSessions() {
  const el = document.getElementById('ov-sessions');
  const s = sessionsData;
  if (!s) {
    el.innerHTML = emptyHtml(sessionsFetched ? 'Session stats unavailable.' : 'Loading…');
    document.getElementById('ov-sessions-meta').textContent = '';
    return;
  }
  document.getElementById('ov-sessions-meta').textContent = 'since last restart';
  el.innerHTML = `<div class="kpis">
      <div class="kpi-big">${fmtInt(s.active)}<small>active now</small></div>
      <div class="kpi">${fmtInt(s.total_opened)}<small>opened</small></div>
      <div class="kpi">${fmtInt(s.total_expired)}<small>expired</small></div>
      <div class="kpi">${fmtInt(s.total_closed)}<small>closed</small></div>
    </div>
    <div class="kpi-note">A Slack thread stays open for ${escapeHtml(s.session_ttl || '—')} after the last reply.</div>`;
}

function renderFleet() {
  const el = document.getElementById('ov-fleet');
  const chat = agentsData.filter(a => a.chat_enabled).length;
  const rows = [];
  rows.push(fleetRow('/ui/agents', 'agents', 'Agents', chat ? `${chat} with chat enabled` : 'Slack only', fmtInt(agentsData.length), agentsData.length ? '' : 'off'));

  if (integrationsData) {
    const total = integrationsData.length;
    const ok = integrationsData.filter(i => i.configured).length;
    rows.push(fleetRow('/ui/integrations', 'integrations', 'Integrations', ok === total ? 'all connected' : `${total - ok} not configured`, `${ok} / ${total}`, ok === total ? '' : ok ? 'warn' : 'bad'));
  } else {
    rows.push(fleetRow('/ui/integrations', 'integrations', 'Integrations', 'loading', '—', 'off'));
  }

  if (workflowsData) {
    const total = workflowsData.length;
    const enabled = workflowsData.filter(w => w.enabled).length;
    const failing = workflowsData.filter(w => w.last_error).length;
    const running = workflowsData.filter(w => w.running).length;
    const sub = [running ? `${running} running` : '', failing ? `${failing} failing` : '', total - enabled ? `${total - enabled} paused` : ''].filter(Boolean).join(' · ') || 'all healthy';
    rows.push(fleetRow('/ui/workflows', 'workflows', 'Workflows', total ? sub : 'none yet', `${enabled} / ${total}`, failing ? 'bad' : total - enabled ? 'warn' : total ? '' : 'off'));
  } else {
    rows.push(fleetRow('/ui/workflows', 'workflows', 'Workflows', 'loading', '—', 'off'));
  }

  if (dashboardsData) {
    const templates = dashboardsData.filter(d => d.kind === 'prompt' && !d.template_id).length;
    const instances = dashboardsData.filter(d => d.template_id).length;
    const failing = dashboardsData.filter(d => d.last_error).length;
    const parts = [templates ? plural(templates, 'template') : '', instances ? plural(instances, 'rendered report') : '', failing ? `${failing} failing` : ''].filter(Boolean).join(' · ');
    rows.push(fleetRow('/ui/dashboards', 'dashboards', 'Dashboards', dashboardsData.length ? (parts || 'source dashboards') : 'none yet', fmtInt(dashboardsData.length), failing ? 'bad' : dashboardsData.length ? '' : 'off'));
  } else {
    rows.push(fleetRow('/ui/dashboards', 'dashboards', 'Dashboards', 'loading', '—', 'off'));
  }
  if (mcpData && mcpData.list) {
    const list = mcpData.list;
    const enabled = list.filter(c => c.enabled).length;
    const failing = list.filter(c => c.enabled && c.last_error).length;
    const tools = list.filter(c => c.enabled && !c.last_error).reduce((n, c) => n + (c.tools || []).length, 0);
    const sub = list.length ? ([tools ? plural(tools, 'tool') : '', failing ? `${failing} failing` : ''].filter(Boolean).join(' · ') || 'no tools discovered') : 'none yet';
    rows.push(fleetRow('/ui/mcp', 'mcp', 'MCP connectors', sub, `${enabled} / ${list.length}`, failing ? 'bad' : !list.length ? 'off' : tools ? '' : 'warn'));
  }
  el.innerHTML = rows.join('');
}

function fleetRow(href, page, label, sub, value, dot) {
  return `<div class="fleet-row">
      <div><a href="${href}" data-page="${page}">${escapeHtml(label)}</a><div class="fleet-sub">${escapeHtml(sub)}</div></div>
      <span class="fleet-val"><span class="dot ${dot}"></span>${escapeHtml(value)}</span>
    </div>`;
}

function renderRecent(sum) {
  const el = document.getElementById('ov-recent');
  const names = userNamesFrom(sum);
  const all = sum.recent || [];
  const evs = all.slice(0, 10);
  document.getElementById('ov-recent-meta').textContent = all.length ? `${evs.length} of ${all.length}` : '';
  if (!evs.length) { el.innerHTML = emptyHtml('No turns recorded yet.'); return; }
  const who = e => {
    if (e.workflow_name || e.workflow_id) return `${escapeHtml(e.workflow_name || e.workflow_id)}<span class="sub">scheduled</span>`;
    if (e.user_id) return `<span title="${escapeHtml(e.user_id)}">${escapeHtml(names.get(e.user_id) || e.user_id)}</span>`;
    return '<span class="muted">—</span>';
  };
  el.innerHTML = `<table class="data-table"><thead><tr><th>When</th><th>Who</th><th>Agent</th><th>Source</th><th class="n">Tokens</th><th class="n">Cost</th></tr></thead><tbody>${
    evs.map(e => `<tr>
      <td class="muted" title="${escapeHtml(new Date(e.at).toLocaleString())}">${timeAgo(e.at)}</td>
      <td>${who(e)}</td>
      <td>${agentChip(e.agent)}</td>
      <td><span class="tag ${escapeHtml(e.source)}">${escapeHtml(e.source)}</span></td>
      <td class="n">${tok(e.total_tokens)}</td>
      <td class="n cost">${money4(e.cost_usd)}</td>
    </tr>`).join('')}</tbody></table>`;
}

function renderLatestChange() {
  const el = document.getElementById('ov-change');
  if (changesState.error) { el.innerHTML = emptyHtml(changesState.error.status === 503 ? 'GitHub is not configured, so the changelog is unavailable.' : 'Changelog unavailable right now.'); return; }
  if (!changesState.list) { el.innerHTML = emptyHtml('Loading…'); return; }
  const c = changesState.list[0];
  if (!c) { el.innerHTML = emptyHtml('No commits found.'); return; }
  el.innerHTML = `<a class="change-sha" href="${escapeHtml(safeExternalUrl(c.url) || '#')}" target="_blank" rel="noopener">${escapeHtml(c.sha)}</a>
    <div class="change-msg">${escapeHtml(c.message)}</div>
    <div class="change-meta">${escapeHtml(c.author)}${c.date ? ' · ' + timeAgo(c.date) : ''}</div>`;
}

/* Workflows page */
function filterPills(el, list, current, attr) {
  const agents = [...new Set(list.map(x => x.agent))].sort((a, b) => agentLabel(a).localeCompare(agentLabel(b)));
  if (agents.length < 2) { el.innerHTML = ''; return; }
  el.innerHTML = `<button class="pill${current === 'all' ? ' active' : ''}" ${attr}="all">All agents</button>` +
    agents.map(a => `<button class="pill${current === a ? ' active' : ''}" ${attr}="${escapeHtml(a)}">${escapeHtml(agentLabel(a))}</button>`).join('');
}

document.getElementById('wf-filter').addEventListener('click', e => {
  const b = e.target.closest('.pill');
  if (!b) return;
  wfFilter = b.dataset.agent;
  renderWorkflowsPage();
});

document.getElementById('dash-filter').addEventListener('click', e => {
  const b = e.target.closest('.pill');
  if (!b) return;
  dashFilter = b.dataset.agent;
  renderDashboardsPage();
});

function gitopsHtml(kind) {
  const g = gitops[kind];
  if (!g || !g.enabled) return '';
  const s = g.status || {};
  const synced = s.last_sync && new Date(s.last_sync).getTime() > 0 ? 'synced ' + timeAgo(s.last_sync) : 'not synced yet';
  return `<span>GitOps</span>
    <span>${escapeHtml(s.owner)}/${escapeHtml(s.repo)}@${escapeHtml(s.branch)}</span>
    <span>${plural(s.managed || 0, 'managed item')}</span>
    <span>${s.running ? 'syncing…' : synced}</span>
    ${s.last_error ? `<span class="err" title="${escapeHtml(s.last_error)}">last sync failed</span>` : ''}
    <button class="btn-mini" type="button" onclick="syncGitops('${kind}', this)"${s.running ? ' disabled' : ''}>Sync now</button>`;
}

async function syncGitops(kind, btn) {
  btn.disabled = true;
  btn.textContent = 'Syncing…';
  try {
    const r = await fetch(`/api/${kind}/_gitops/sync`, { method: 'POST' });
    if (!r.ok) throw new Error((await r.text()) || ('HTTP ' + r.status));
  } catch (err) {
    alert('Sync failed: ' + err);
  }
  delete lastFetched[kind];
  await Promise.all([loadGitops(kind, true), kind === 'workflows' ? loadWorkflows() : loadDashboards()]);
}

function wfStatus(w) {
  if (w.running) return ['running', 'running', ''];
  if (!w.enabled) return w.disabled_reason ? ['auto', 'auto-paused', w.disabled_reason] : ['paused', 'paused', ''];
  if (w.last_error) return ['failed', 'failed', w.last_error];
  if (w.last_run) return ['ok', 'ok', ''];
  return ['paused', 'not run yet', ''];
}

function wfSchedule(w) {
  const type = (w.trigger && w.trigger.type) || (w.cron ? 'schedule' : 'manual');
  if (type === 'schedule') return w.cron ? `<span class="tag">${escapeHtml(w.cron)}</span>` : '<span class="muted">manual</span>';
  if (type === 'on_success' || type === 'on_failure') {
    return `<span class="muted">${type === 'on_success' ? 'after success of' : 'after failure of'}</span><span class="sub">${escapeHtml((w.trigger && w.trigger.ref) || '')}</span>`;
  }
  return `<span class="muted">${escapeHtml(type)}</span>`;
}

function renderWorkflowsPage() {
  const el = document.getElementById('wf-list');
  document.getElementById('wf-gitops').innerHTML = gitopsHtml('workflows');
  if (!workflowsData) return;
  filterPills(document.getElementById('wf-filter'), workflowsData, wfFilter, 'data-agent');
  if (wfFilter !== 'all' && !workflowsData.some(w => w.agent === wfFilter)) wfFilter = 'all';
  const rows = workflowsData.filter(w => wfFilter === 'all' || w.agent === wfFilter)
    .sort((a, b) => agentLabel(a.agent).localeCompare(agentLabel(b.agent)) || (a.name || '').localeCompare(b.name || ''));
  if (!rows.length) {
    el.innerHTML = emptyHtml(workflowsData.length ? 'No workflows for this agent.' : 'No workflows yet. Ask an agent in Slack to create one, or add a descriptor to the GitOps repo.', true);
    return;
  }
  el.innerHTML = `<table class="data-table"><thead><tr>
      <th>Workflow</th><th>Agent</th><th>Schedule</th><th>Status</th><th>Last run</th><th></th>
    </tr></thead><tbody>${rows.map(w => {
      const [cls, label, title] = wfStatus(w);
      const url = `/${encodeURIComponent(w.agent)}/workflow/${encodeURIComponent(w.id)}`;
      const managed = w.source === 'gitops';
      return `<tr>
        <td><a href="${url}" target="_blank" rel="noopener">${escapeHtml(w.name || w.id)}</a>${managed ? ' <span class="tag gitops" title="Managed from git; read-only here">gitops</span>' : ''}
          <span class="sub" title="${escapeHtml(w.description || '')}">${escapeHtml(w.short_name || w.id)}${w.description ? ' · ' + escapeHtml(w.description) : ''}</span></td>
        <td>${agentChip(w.agent)}</td>
        <td>${wfSchedule(w)}</td>
        <td><span class="status-pill ${cls}" title="${escapeHtml(title)}">${label}</span></td>
        <td class="muted" title="${w.last_run ? escapeHtml(new Date(w.last_run).toLocaleString()) : ''}">${w.last_run ? timeAgo(w.last_run) : '—'}</td>
        <td><div class="actions">
          <a class="btn-mini" href="${url}" target="_blank" rel="noopener">Open</a>
          <button class="btn-mini" type="button" onclick="runWorkflow('${w.agent}','${w.id}', this)"${w.running ? ' disabled' : ''}>Run</button>
          ${managed ? '' : `<button class="btn-mini danger" type="button" onclick="deleteWorkflow('${w.agent}','${w.id}')">Delete</button>`}
        </div></td>
      </tr>`;
    }).join('')}</tbody></table>`;
}

async function runWorkflow(agentId, id, btn) {
  if (!confirm('Run this workflow now? It uses the agent’s LLM tool loop and counts toward usage.')) return;
  if (btn) { btn.disabled = true; btn.textContent = 'Queued'; }
  try {
    const r = await fetch(`/api/workflows/${encodeURIComponent(agentId)}/${encodeURIComponent(id)}/run`, { method: 'POST' });
    if (!r.ok) throw new Error((await r.text()) || ('HTTP ' + r.status));
  } catch (err) {
    alert('Failed to start workflow: ' + err);
  }
  delete lastFetched.workflows;
  setTimeout(loadWorkflows, 1500);
}

async function deleteWorkflow(agentId, id) {
  if (!confirm('Delete this workflow? This stops its schedule and removes its stored data.')) return;
  try {
    const r = await fetch(`/api/workflows/${encodeURIComponent(agentId)}/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (!r.ok && r.status !== 204) throw new Error('HTTP ' + r.status);
    delete lastFetched.workflows;
    await loadWorkflows();
  } catch (err) {
    alert('Failed to delete workflow: ' + err);
  }
}

/* Dashboards page */
function promptInputs(prompt) {
  const out = new Set();
  const re = /\{\{\s*([A-Za-z0-9_]+)\s*\}\}/g;
  let m;
  while ((m = re.exec(prompt || ''))) out.add(m[1]);
  return [...out];
}

function dashKind(d, byId) {
  if (d.template_id) {
    const t = byId.get(d.agent + '/' + d.template_id);
    return `<span class="muted">rendered report</span><span class="sub">of ${escapeHtml(t ? (t.short_name || t.name) : d.template_id)}</span>`;
  }
  if (d.kind === 'prompt') {
    const inputs = promptInputs(d.prompt);
    return `<span class="muted">prompt template</span><span class="sub">${inputs.length ? plural(inputs.length, 'input') + ': ' + escapeHtml(inputs.join(', ')) : 'self-contained'}</span>`;
  }
  const n = (d.sources || []).length;
  return `<span class="muted">${plural(n, 'source')}</span><span class="sub">${escapeHtml([...new Set((d.sources || []).map(s => s.type))].join(', '))}</span>`;
}

function dashStatus(d) {
  if (d.last_error) return ['failed', 'failed', d.last_error];
  if (d.last_sync) return ['ok', 'ok', ''];
  if (d.kind === 'prompt' && !d.template_id && promptInputs(d.prompt).length) return ['paused', 'awaiting inputs', ''];
  return ['paused', 'not synced yet', ''];
}

function renderDashboardsPage() {
  const el = document.getElementById('dash-list');
  document.getElementById('dash-gitops').innerHTML = gitopsHtml('dashboards');
  if (!dashboardsData) return;
  filterPills(document.getElementById('dash-filter'), dashboardsData, dashFilter, 'data-agent');
  if (dashFilter !== 'all' && !dashboardsData.some(d => d.agent === dashFilter)) dashFilter = 'all';
  const byId = new Map(dashboardsData.map(d => [d.agent + '/' + d.id, d]));
  const rank = d => (d.template_id ? 2 : d.kind === 'prompt' ? 1 : 0);
  const rows = dashboardsData.filter(d => dashFilter === 'all' || d.agent === dashFilter)
    .sort((a, b) => agentLabel(a.agent).localeCompare(agentLabel(b.agent)) || rank(a) - rank(b) || (a.name || '').localeCompare(b.name || ''));
  if (!rows.length) {
    el.innerHTML = emptyHtml(dashboardsData.length ? 'No dashboards for this agent.' : 'No dashboards yet. Ask an agent in Slack to create one, or add a descriptor to the GitOps repo.', true);
    return;
  }
  el.innerHTML = `<table class="data-table"><thead><tr>
      <th>Dashboard</th><th>Agent</th><th>Kind</th><th>Refresh</th><th>Status</th><th>Last sync</th><th></th>
    </tr></thead><tbody>${rows.map(d => {
      const [cls, label, title] = dashStatus(d);
      const url = `/${encodeURIComponent(d.agent)}/dashboard/${encodeURIComponent(d.id)}`;
      const managed = d.source === 'gitops';
      return `<tr>
        <td><a href="${url}" target="_blank" rel="noopener">${escapeHtml(d.name || d.id)}</a>${managed ? ' <span class="tag gitops" title="Managed from git; read-only here">gitops</span>' : ''}
          <span class="sub" title="${escapeHtml(d.description || '')}">${escapeHtml(d.short_name || d.id)}${d.description ? ' · ' + escapeHtml(d.description) : ''}</span></td>
        <td>${agentChip(d.agent)}</td>
        <td>${dashKind(d, byId)}</td>
        <td><span class="tag">${escapeHtml(d.sync_interval || '—')}</span></td>
        <td><span class="status-pill ${cls}" title="${escapeHtml(title)}">${label}</span></td>
        <td class="muted" title="${d.last_sync ? escapeHtml(new Date(d.last_sync).toLocaleString()) : ''}">${d.last_sync ? timeAgo(d.last_sync) : '—'}</td>
        <td><div class="actions">
          <a class="btn-mini" href="${url}" target="_blank" rel="noopener">Open</a>
          ${managed ? '' : `<button class="btn-mini danger" type="button" onclick="deleteDashboard('${d.agent}','${d.id}')">Delete</button>`}
        </div></td>
      </tr>`;
    }).join('')}</tbody></table>`;
}

async function deleteDashboard(agentId, id) {
  if (!confirm('Delete this dashboard? This stops its sync and removes its stored data.')) return;
  try {
    const r = await fetch(`/api/dashboards/${encodeURIComponent(agentId)}/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (!r.ok && r.status !== 204) throw new Error('HTTP ' + r.status);
    delete lastFetched.dashboards;
    await loadDashboards();
  } catch (err) {
    alert('Failed to delete dashboard: ' + err);
  }
}

/* Changelog */
function renderChanges() {
  const list = document.getElementById('changes-list');
  if (changesState.error) {
    list.innerHTML = `<div class="empty-state" style="padding:30px;"><p>${changesState.error.status === 503 ? 'GitHub is not configured, so the changelog is unavailable.' : 'Failed to load changes.'}</p></div>`;
    return;
  }
  const commits = changesState.list;
  if (!commits) return;
  if (!commits.length) {
    list.innerHTML = '<div class="empty-state" style="padding:30px;"><p>No changes found.</p></div>';
    return;
  }
  list.innerHTML = commits.map(c => `
    <div class="change-item">
      <a class="change-sha" href="${escapeHtml(safeExternalUrl(c.url) || '#')}" target="_blank" rel="noopener">${escapeHtml(c.sha)}</a>
      <div class="change-body">
        <div class="change-message" title="${escapeHtml(c.message)}">${escapeHtml(c.message)}</div>
        <div class="change-meta">${escapeHtml(c.author)} · ${c.date ? timeAgo(c.date) : ''}</div>
      </div>
    </div>`).join('');
}

/* Usage & Billing */
function billingRows(el, data, nameOf) {
  if (!data || !data.length) { el.innerHTML = emptyHtml('No usage in this window'); return; }
  let h = '<table class="data-table"><thead><tr><th>Name</th><th class="n">Req</th><th class="n">Tokens</th><th class="n">Cost</th></tr></thead><tbody>';
  data.slice(0, 12).forEach(r => {
    h += `<tr><td>${nameOf(r)}</td><td class="n">${fmtInt(r.counts.requests)}</td><td class="n">${tok(r.counts.total_tokens)}</td><td class="n cost">${money4(r.counts.cost_usd)}</td></tr>`;
  });
  el.innerHTML = h + '</tbody></table>';
}

function billingRecent(el, evs, names) {
  if (!evs || !evs.length) { el.innerHTML = emptyHtml('No turns recorded yet'); return; }
  let h = '<table class="data-table"><thead><tr><th>Time</th><th>Agent</th><th>Source</th><th>Model</th><th class="n">Tokens</th><th class="n">Saved</th><th class="n">Cost</th></tr></thead><tbody>';
  evs.slice(0, 30).forEach(e => {
    const t = new Date(e.at);
    const who = e.workflow_name ? ` · ${escapeHtml(e.workflow_name)}` : (e.user_id ? ` · ${escapeHtml(names.get(e.user_id) || e.user_id)}` : '');
    const cs = e.compression_saved_tokens || 0, ci = e.compression_input_tokens || 0, cp = ci ? Math.round(cs / ci * 100) : 0;
    const saved = cs ? `${tok(cs)} <small class="muted">${cp}%</small>` : '—';
    h += `<tr><td>${t.toLocaleString()}</td><td>${agentChip(e.agent)}</td><td><span class="tag ${escapeHtml(e.source)}">${escapeHtml(e.source)}</span>${who}</td><td>${escapeHtml(e.model)}</td><td class="n">${tok(e.total_tokens)}</td><td class="n">${saved}</td><td class="n cost">${money4(e.cost_usd)}</td></tr>`;
  });
  el.innerHTML = h + '</tbody></table>';
}

function priceSrc(p) {
  if (!p || (!p.url && !p.last_sync && !p.error)) return 'Pricing: LLM_PRICE_OVERRIDES only · set PRICE_SOURCE_URL for a live feed';
  let host = p.url;
  try { host = new URL(p.url).host; } catch (e) {}
  if (p.error) return `⚠ Pricing source unavailable (${escapeHtml(p.error)}) · ${p.models || 0} rates loaded · overrides win`;
  const sourceUrl = safeExternalUrl(p.url);
  const src = sourceUrl ? `<a href="${escapeHtml(sourceUrl)}" target="_blank" rel="noopener noreferrer">${escapeHtml(host)}</a>` : escapeHtml(host);
  return `Prices: ${src} · ${fmtInt(p.models || 0)} models${p.last_sync ? ' · synced ' + timeAgo(p.last_sync) : ''} · LLM_PRICE_OVERRIDES win`;
}

function renderBilling(d) {
  const t = d.totals || {};
  const $ = s => document.querySelector(s);
  const names = userNamesFrom(d);
  $('#total-cost').textContent = money(t.cost_usd);
  $('#total-sub').textContent = t.requests ? `${fmtInt(t.requests)} requests · ${tok(t.total_tokens)} tokens · ${windowLabel()}` : `No usage recorded · ${windowLabel()}`;
  $('#s-req').textContent = fmtInt(t.requests);
  $('#s-tok').textContent = tok(t.total_tokens);
  $('#s-avg').textContent = money4(t.requests ? t.cost_usd / t.requests : 0);
  const cin = t.cached_prompt_tokens || 0, pin = (t.prompt_tokens || 0) + cin;
  $('#s-cache').textContent = (pin ? Math.round(cin / pin * 100) : 0) + '%';
  const csave = t.compression_saved_tokens || 0, cinp = t.compression_input_tokens || 0;
  $('#s-comp').innerHTML = csave ? `${tok(csave)} <small>${cinp ? Math.round(csave / cinp * 100) : 0}%</small>` : '0';
  renderBars($('#bars'), fillDaily(d.daily, days), r => r.counts.cost_usd || 0, v => money4(v), $('#axis-from'), $('#axis-to'));
  billingRows($('#t-agent'), d.by_agent, r => agentChip(r.key));
  billingRows($('#t-source'), d.by_source, r => `<span class="tag ${escapeHtml(r.key)}">${escapeHtml(r.key)}</span>`);
  billingRows($('#t-user'), d.by_user, r => `<span title="${escapeHtml(r.key)}">${escapeHtml(userLabel(r))}</span>`);
  billingRows($('#t-model'), d.by_model, r => escapeHtml(r.key));
  billingRows($('#t-wf'), d.by_workflow, r => escapeHtml(r.name || r.key) + (r.agent ? ` <small class="muted">${escapeHtml(agentLabel(r.agent))}</small>` : ''));
  billingRecent($('#t-recent'), d.recent, names);
  $('#recent-meta').textContent = d.recent && d.recent.length ? `${d.recent.length} shown` : '';
  $('#price-src').innerHTML = priceSrc(d.pricing);
}

/* Agents */
function renderAgents(agents) {
  const grid = document.getElementById('agents-grid');

  if (agents.length === 0) {
    grid.innerHTML = `
      <div class="empty-state">
        <div class="empty-state-icon">&#x1f916;</div>
        <p>No agents configured yet. Agents will appear here once created.</p>
      </div>`;
    return;
  }

  grid.innerHTML = agents.map((agent, idx) => {
    const color = hashColor(agent.name);
    const initial = agent.name.charAt(0).toUpperCase();
    const promptCount = Object.keys(agent.prompts || {}).length;
    return `
      <div class="agent-card" style="--i:${idx}" data-agent-id="${agent.id}" onclick="openAgent('${agent.id}')">
        <div class="agent-card-header">
          <div class="agent-avatar" style="background:${color}">${initial}</div>
          <div class="agent-info">
            <div class="agent-name">${escapeHtml(agent.name)}</div>
            <div class="agent-profession">${escapeHtml(getAgentProfession(agent))}</div>
          </div>
        </div>
        <div class="agent-status"><span class="status-dot"></span> Active</div>
        <div class="agent-prompts-count">${plural(promptCount, 'prompt')}</div>
        ${agent.chat_enabled ? `<button type="button" class="agent-chat-chip" title="Open chat" onclick="event.stopPropagation(); openChat('${agent.id}')">&#x1f4ac; Open chat</button>` : ''}
        ${renderAgentBadges(agent.id)}
        ${renderAgentDashboards(agent.id)}
        ${renderAgentWorkflows(agent.id)}
      </div>`;
  }).join('');
}

function applyAgentExtras() {
  document.querySelectorAll('#agents-grid .agent-card').forEach(card => {
    // Resolve the card back to the agent record instead of trusting the
    // attribute text, so only ids the API returned reach the markup.
    const agent = agentById(card.getAttribute('data-agent-id'));
    if (!agent) return;
    card.querySelectorAll('.agent-dashboards, .agent-workflows').forEach(el => el.remove());
    card.insertAdjacentHTML('beforeend', renderAgentDashboards(agent.id) + renderAgentWorkflows(agent.id));
  });
}

function renderAgentDashboards(agentId) {
  const aid = safeId(agentId);
  const list = aid ? (AGENT_DASHBOARDS[aid] || []) : [];
  if (list.length === 0) return '';
  const chips = list.map(d => {
    const did = safeId(d.id);
    if (!did) return '';
    const label = escapeHtml(d.short_name || d.name || d.id);
    const url = `/${aid}/dashboard/${did}`;
    const title = escapeHtml(d.name + (d.description ? ' — ' + d.description : ''));
    return `<a class="agent-dashboard-chip" href="${url}" target="_blank" rel="noopener" title="${title}" onclick="event.stopPropagation()">${label}<button class="dash-delete" title="Delete dashboard" onclick="event.preventDefault();event.stopPropagation();deleteDashboard('${aid}','${did}');">×</button></a>`;
  }).join('');
  return `<div class="agent-dashboards"><div class="agent-dashboards-label">Available dashboards</div><div class="agent-dashboards-list">${chips}</div></div>`;
}

function renderAgentWorkflows(agentId) {
  const aid = safeId(agentId);
  const list = aid ? (AGENT_WORKFLOWS[aid] || []) : [];
  if (list.length === 0) return '';
  const chips = list.map(w => {
    const wid = safeId(w.id);
    if (!wid) return '';
    const label = escapeHtml(w.short_name || w.name || w.id);
    const url = `/${aid}/workflow/${wid}`;
    const title = escapeHtml(w.name + (w.description ? ' — ' + w.description : '') + ' · cron ' + (w.cron || '—'));
    const cls = w.last_error ? ' err' : '';
    return `<a class="agent-workflow-chip${cls}" href="${url}" target="_blank" rel="noopener" title="${title}" onclick="event.stopPropagation()">${label}<button class="wf-delete" title="Delete workflow" onclick="event.preventDefault();event.stopPropagation();deleteWorkflow('${aid}','${wid}');">×</button></a>`;
  }).join('');
  return `<div class="agent-workflows"><div class="agent-workflows-label">Scheduled workflows</div><div class="agent-workflows-list">${chips}</div></div>`;
}

function renderAgentBadges(agentId) {
  const integrations = AGENT_INTEGRATIONS[agentId];
  if (!integrations || integrations.length === 0) return '';
  const badges = integrations.map(ig => {
    const logo = getIntegrationLogo(ig.id);
    const cls = ig.planned ? ' planned' : '';
    return `<span class="agent-integration-badge${cls}" title="${escapeHtml(ig.name)}${ig.planned ? ' (planned)' : ''}">${logo}<span class="badge-label">${escapeHtml(ig.name)}</span>${ig.planned ? '<span class="planned-tag">soon</span>' : ''}</span>`;
  }).join('');
  return `<div class="agent-integrations">${badges}</div>`;
}

function openAgent(id) {
  const agent = agentById(id);
  if (!agent) return;
  const color = hashColor(agent.name);
  document.getElementById('modal-avatar').style.background = color;
  document.getElementById('modal-avatar').textContent = agent.name.charAt(0).toUpperCase();
  document.getElementById('modal-title').textContent = agent.name;
  document.getElementById('modal-subtitle').textContent = getAgentProfession(agent);

  const body = document.getElementById('modal-body');
  const prompts = agent.prompts || {};
  const keys = Object.keys(prompts);
  const integrationsHtml = renderModalIntegrations(agent.id);
  if (keys.length === 0) {
    body.innerHTML = integrationsHtml + '<p style="color:var(--text-muted);font-size:13px;">No prompts configured for this agent.</p>';
  } else {
    body.innerHTML = integrationsHtml + keys.map(key => `
      <div class="prompt-section">
        <div class="prompt-label">${escapeHtml(key)}</div>
        <div class="prompt-content">${escapeHtml(prompts[key])}</div>
      </div>`).join('');
  }
  document.getElementById('modal-overlay').classList.add('active');
}

function renderModalIntegrations(agentId) {
  const integrations = AGENT_INTEGRATIONS[agentId];
  if (!integrations || integrations.length === 0) return '';
  const items = integrations.map(ig => {
    const logo = getIntegrationLogo(ig.id);
    const cls = ig.planned ? ' planned' : '';
    const statusText = ig.planned ? 'Planned' : 'Active';
    return `
      <div class="modal-integration-item${cls}">
        ${logo}
        <div class="mi-info">
          <span class="mi-name">${escapeHtml(ig.name)}</span>
          <span class="mi-status">${ig.planned ? '◌' : '●'} ${statusText}</span>
        </div>
      </div>`;
  }).join('');
  return `
    <div class="modal-integrations-section">
      <div class="modal-integrations-title">Integrations</div>
      <div class="modal-integrations-grid">${items}</div>
    </div>`;
}

function closeModal() { document.getElementById('modal-overlay').classList.remove('active'); }

document.getElementById('modal-close').addEventListener('click', closeModal);
document.getElementById('modal-overlay').addEventListener('click', e => {
  if (e.target === document.getElementById('modal-overlay')) closeModal();
});

/* Chat: slide-over panel and full-screen view */
let chatAgentId = null;
let chatConvId = null;
let chatCurrentEmpty = true;
let chatBusy = false;
let chatFull = false;

function chatEls() {
  return chatFull
    ? { box: document.getElementById('fs-chat-messages'), input: document.getElementById('fs-chat-input'), send: document.getElementById('fs-chat-send') }
    : { box: document.getElementById('chat-messages'), input: document.getElementById('chat-input'), send: document.getElementById('chat-send') };
}

function convBase(agent) { return `/api/chat/${encodeURIComponent(agent)}/conversations`; }

async function apiListConversations(agent) { return fetchJSON(convBase(agent)); }
async function apiCreateConversation(agent) { return fetchJSON(convBase(agent), { method: 'POST' }); }
async function apiGetConversation(agent, id) { return fetchJSON(`${convBase(agent)}/${encodeURIComponent(id)}`); }
async function apiDeleteConversation(agent, id) {
  const r = await fetch(`${convBase(agent)}/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (!r.ok && r.status !== 204) throw new Error('HTTP ' + r.status);
}
async function apiRenameConversation(agent, id, title) {
  return fetchJSON(`${convBase(agent)}/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title }),
  });
}

async function openChat(id) {
  const agent = agentById(id);
  if (!agent) return;
  chatAgentId = id;
  chatConvId = null;
  chatFull = false;
  const avatar = document.getElementById('chat-avatar');
  avatar.style.background = hashColor(agent.name);
  avatar.textContent = agent.name.charAt(0).toUpperCase();
  document.getElementById('chat-title').textContent = agent.name;
  document.getElementById('chat-subtitle').textContent = getAgentProfession(agent);
  document.getElementById('chat-messages').innerHTML = '<div class="chat-empty">Loading conversation…</div>';
  document.getElementById('chat-input').value = '';
  document.getElementById('chat-overlay').classList.add('active');
  await openLatestOrNew(id);
  setTimeout(() => document.getElementById('chat-input').focus(), 120);
}

function closeChat() {
  document.getElementById('chat-overlay').classList.remove('active');
  chatAgentId = null;
  chatConvId = null;
}

function setChatInputVisible(visible) {
  const { input } = chatEls();
  const bar = input ? input.closest('.chat-input-bar') : null;
  if (bar) bar.style.display = visible ? '' : 'none';
}

function showChatAccessDenied() {
  setChatInputVisible(false);
  chatEls().box.innerHTML =
    '<div class="chat-empty chat-denied">' +
    '<div class="chat-denied-icon">&#x1f512;</div>' +
    '<div class="chat-denied-title">You don’t have access to this chat</div>' +
    '<div class="chat-denied-text">Your account isn’t authorized to use this agent. ' +
    'Contact your administrator if you need access.</div>' +
    '</div>';
  if (chatFull) {
    const list = document.getElementById('fs-conv-list');
    if (list) list.innerHTML = '';
  }
}

async function openLatestOrNew(agent, convId) {
  setChatInputVisible(true);
  try {
    const list = await apiListConversations(agent);
    if (chatAgentId !== agent) return;
    if (chatFull) renderConvList(list);
    if (convId && list.some(c => c.id === convId)) await selectConversation(agent, convId);
    else if (list.length > 0) await selectConversation(agent, list[0].id);
    else await newChat(agent);
  } catch (err) {
    if (chatAgentId !== agent) return;
    if (err && err.status === 403) { showChatAccessDenied(); return; }
    chatEls().box.innerHTML = `<div class="chat-empty">Failed to load conversations: ${escapeHtml(String(err))}</div>`;
  }
}

function populateAgentSelect() {
  const sel = document.getElementById('fs-agent-select');
  sel.innerHTML = agentsData.filter(a => a.chat_enabled)
    .map(a => `<option value="${a.id}">${escapeHtml(a.name)}</option>`).join('');
  if (chatAgentId) sel.value = chatAgentId;
}

// Built with DOM nodes rather than markup: titles are user text and the agent
// id can come from the URL, so neither is ever parsed as HTML.
function renderConvList(list) {
  const box = document.getElementById('fs-conv-list');
  box.replaceChildren();
  if (!list || list.length === 0) {
    box.appendChild(elem('div', 'chat-empty', 'No conversations yet.'));
    return;
  }
  const agent = chatAgentId;
  for (const c of list) {
    const row = elem('div', 'fs-conv' + (c.id === chatConvId ? ' active' : ''));
    row.addEventListener('click', () => selectConversation(agent, c.id));

    const info = elem('div', 'fs-conv-info');
    info.append(
      elem('div', 'fs-conv-name', c.title || 'New chat'),
      elem('div', 'fs-conv-sub', `${timeAgo(c.updated_at)} · ${plural(c.message_count, 'msg')}`),
    );

    const rename = elem('button', 'fs-conv-action', '\u270e');
    rename.type = 'button';
    rename.title = 'Rename';
    rename.addEventListener('click', e => { e.stopPropagation(); renameConversationPrompt(c.id, rename); });

    const remove = elem('button', 'fs-conv-action', '\u{1f5d1}');
    remove.type = 'button';
    remove.title = 'Delete';
    remove.addEventListener('click', e => { e.stopPropagation(); removeConversation(c.id); });

    row.append(info, rename, remove);
    box.appendChild(row);
  }
}

async function refreshConvList(agent) {
  if (!chatFull) return;
  try {
    const list = await apiListConversations(agent);
    if (chatAgentId === agent) renderConvList(list);
  } catch (err) {
    console.warn('Failed to refresh conversation list:', err);
  }
}

async function selectFullChatAgent(id, convId) {
  const agent = agentById(id);
  if (!agent) return;
  id = agent.id;
  chatAgentId = id;
  chatConvId = null;
  chatFull = true;
  const avatar = document.getElementById('fs-chat-avatar');
  avatar.style.background = hashColor(agent.name);
  avatar.textContent = agent.name.charAt(0).toUpperCase();
  document.getElementById('fs-chat-title').textContent = agent.name;
  document.getElementById('fs-chat-subtitle').textContent = getAgentProfession(agent);
  document.getElementById('fs-chat-messages').innerHTML = '<div class="chat-empty">Loading…</div>';
  document.getElementById('fs-chat-input').value = '';
  document.getElementById('fs-agent-select').value = id;
  const path = '/ui/' + encodeURIComponent(id) + '/chat';
  if (location.pathname !== path) history.pushState({}, '', path);
  document.title = `${appTitle} — ${agent.name} chat`;
  await openLatestOrNew(id, convId);
  setTimeout(() => document.getElementById('fs-chat-input').focus(), 80);
}

function openFullChat(id, convId) {
  if (!agentById(id)) return;
  if (!chatFull && currentPage) pageBeforeChat = currentPage;
  chatFull = true;
  document.getElementById('chat-overlay').classList.remove('active');
  document.getElementById('chat-fullscreen').classList.add('active');
  populateAgentSelect();
  selectFullChatAgent(id, convId);
}

function hideFullChat() {
  document.getElementById('chat-fullscreen').classList.remove('active');
  chatFull = false;
  chatAgentId = null;
  chatConvId = null;
}

function closeFullChat() {
  hideFullChat();
  navigate(pageBeforeChat || 'agents');
}

function expandChat() {
  if (chatAgentId) openFullChat(chatAgentId);
}

function collapseChat() {
  const id = chatAgentId;
  hideFullChat();
  navigate(pageBeforeChat || 'agents');
  if (id) openChat(id);
}

async function selectConversation(agent, convId) {
  chatAgentId = agent;
  chatConvId = convId;
  try {
    const conv = await apiGetConversation(agent, convId);
    if (chatAgentId !== agent || chatConvId !== convId) return;
    renderChatMessages(conv.messages || []);
    if (chatFull) document.querySelectorAll('#fs-conv-list .fs-conv').forEach(el => el.classList.remove('active'));
    await refreshConvList(agent);
  } catch (err) {
    chatEls().box.innerHTML = `<div class="chat-empty">Failed to load conversation: ${escapeHtml(String(err))}</div>`;
  }
  const { input } = chatEls();
  if (input) setTimeout(() => input.focus(), 40);
}

async function newChat(agent) {
  if (chatConvId && chatCurrentEmpty) {
    const { input } = chatEls();
    if (input) input.focus();
    return;
  }
  try {
    const conv = await apiCreateConversation(agent);
    chatAgentId = agent;
    chatConvId = conv.id;
    chatCurrentEmpty = true;
    renderChatMessages([]);
    await refreshConvList(agent);
    const { input } = chatEls();
    if (input) input.focus();
  } catch (err) {
    chatEls().box.innerHTML = `<div class="chat-empty">Failed to start a new chat: ${escapeHtml(String(err))}</div>`;
  }
}

async function removeConversation(convId) {
  if (!confirm('Delete this conversation? This removes its history for everyone.')) return;
  const agent = chatAgentId;
  try {
    await apiDeleteConversation(agent, convId);
    if (chatConvId === convId) chatConvId = null;
    await openLatestOrNew(agent);
  } catch (err) {
    alert('Failed to delete conversation: ' + err);
  }
}

async function renameConversationPrompt(convId, btn) {
  let current = '';
  const row = btn && btn.closest ? btn.closest('.fs-conv') : null;
  if (row) {
    const nameEl = row.querySelector('.fs-conv-name');
    if (nameEl) current = nameEl.textContent || '';
  }
  const title = prompt('Rename conversation', current.trim());
  if (title == null) return;
  const trimmed = title.trim();
  if (!trimmed) return;
  try {
    await apiRenameConversation(chatAgentId, convId, trimmed);
    await refreshConvList(chatAgentId);
  } catch (err) {
    alert('Failed to rename conversation: ' + err);
  }
}

function chatBubbleHtml(m) {
  const who = m.role === 'assistant' ? 'assistant' : 'user';
  const name = m.role === 'assistant' ? '' : (m.user ? escapeHtml(m.user) : 'Anonymous');
  const meta = name ? `<div class="chat-meta">${name}</div>` : '';
  const cls = m.pending ? ' pending' : (m.error ? ' chat-error' : '');
  const body = (who === 'assistant' && !m.pending && !m.error) ? renderSlackMarkdown(m.content) : escapeHtml(m.content);
  return `<div class="chat-msg ${who}">${meta}<div class="chat-bubble${cls}">${body}</div></div>`;
}

function renderChatMessages(msgs) {
  chatCurrentEmpty = !msgs || msgs.length === 0;
  const box = chatEls().box;
  if (chatCurrentEmpty) {
    box.innerHTML = '<div class="chat-empty">No messages yet. Start the conversation.</div>';
    return;
  }
  box.innerHTML = msgs.map(chatBubbleHtml).join('');
  box.scrollTop = box.scrollHeight;
}

function appendChatBubble(m) {
  const box = chatEls().box;
  const empty = box.querySelector('.chat-empty');
  if (empty) box.innerHTML = '';
  box.insertAdjacentHTML('beforeend', chatBubbleHtml(m));
  box.scrollTop = box.scrollHeight;
}

async function sendChat() {
  if (chatBusy || !chatAgentId) return;
  const agent = chatAgentId;
  if (!chatConvId) {
    try {
      const conv = await apiCreateConversation(agent);
      chatConvId = conv.id;
      chatCurrentEmpty = true;
    } catch (err) {
      chatEls().box.innerHTML = `<div class="chat-empty">Failed to start a chat: ${escapeHtml(String(err))}</div>`;
      return;
    }
  }
  const convId = chatConvId;
  const { input, send: sendBtn } = chatEls();
  const text = input.value.trim();
  if (!text) return;

  chatBusy = true;
  sendBtn.disabled = true;
  input.disabled = true;
  appendChatBubble({ role: 'user', content: text });
  appendChatBubble({ role: 'assistant', content: 'Thinking…', pending: true });
  input.value = '';
  chatCurrentEmpty = false;

  try {
    const r = await fetch(`${convBase(agent)}/${encodeURIComponent(convId)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text }),
    });
    if (!r.ok) throw new Error((await r.text()) || ('HTTP ' + r.status));
    if (chatAgentId === agent && chatConvId === convId) {
      const conv = await apiGetConversation(agent, convId);
      renderChatMessages(conv.messages || []);
      await refreshConvList(agent);
    }
  } catch (err) {
    if (chatAgentId === agent && chatConvId === convId) {
      try {
        const conv = await apiGetConversation(agent, convId);
        renderChatMessages(conv.messages || []);
      } catch (_) {}
      appendChatBubble({ role: 'assistant', content: 'Error: ' + String(err).slice(0, 300), error: true });
    }
  } finally {
    chatBusy = false;
    sendBtn.disabled = false;
    input.disabled = false;
    input.focus();
  }
}

document.getElementById('chat-close').addEventListener('click', closeChat);
document.getElementById('chat-new').addEventListener('click', () => { if (chatAgentId) newChat(chatAgentId); });
document.getElementById('chat-overlay').addEventListener('click', e => {
  if (e.target === document.getElementById('chat-overlay')) closeChat();
});
document.getElementById('chat-send').addEventListener('click', sendChat);
document.getElementById('chat-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendChat(); }
});
document.getElementById('chat-expand').addEventListener('click', expandChat);
document.getElementById('fs-collapse').addEventListener('click', collapseChat);
document.getElementById('fs-back').addEventListener('click', closeFullChat);
document.getElementById('fs-new-chat').addEventListener('click', () => { if (chatAgentId) newChat(chatAgentId); });
document.getElementById('fs-agent-select').addEventListener('change', e => selectFullChatAgent(e.target.value));
document.getElementById('fs-chat-send').addEventListener('click', sendChat);
document.getElementById('fs-chat-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendChat(); }
});

document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (!document.getElementById('user-pop').hidden) { setIdentityOpen(false); return; }
  if (document.getElementById('form-overlay').classList.contains('active')) { closeForm(); return; }
  if (document.documentElement.dataset.drawer === 'open') { closeDrawer(); return; }
  if (chatFull) { closeFullChat(); return; }
  closeChat();
  closeModal();
});

function renderSlackMarkdown(text) {
  if (text == null) return '';
  const tokens = [];
  const stash = (html) => `\u0000${tokens.push(html) - 1}\u0000`;
  const escAttr = (s) => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;');
  let s = String(text);
  s = s.replace(/<(https?:\/\/[^|>\s]+)\|([^>]+)>/g, (_, url, label) =>
    stash(`<a href="${escAttr(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(label)}</a>`));
  s = s.replace(/<(https?:\/\/[^|>\s]+)>/g, (_, url) =>
    stash(`<a href="${escAttr(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(url)}</a>`));
  s = s.replace(/`([^`]+)`/g, (_, code) => stash(`<code>${escapeHtml(code)}</code>`));
  s = escapeHtml(s);
  s = s.replace(/\*([^*\n]+)\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[\s(])_([^_\n]+)_(?=[\s).,!?:;]|$)/g, '$1<em>$2</em>');
  s = s.replace(/~([^~\n]+)~/g, '<del>$1</del>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => tokens[Number(i)]);
}

/* Integrations */
let expandedIntegration = null;
let activeIntegrationTab = 'permissions';

function renderIntegrations(integrations) {
  const grid = document.getElementById('integrations-grid');
  const detailPanel = document.getElementById('integration-detail');
  const count = document.getElementById('integration-count');
  const connected = integrations.filter(i => i.configured).length;
  count.textContent = integrations.length ? `${connected} of ${integrations.length} connected` : '';
  if (integrations.length === 0) {
    grid.innerHTML = `<div class="empty-state" style="grid-column:1/-1;padding:30px;"><p>No integrations found.</p></div>`;
    detailPanel.innerHTML = '';
    return;
  }

  grid.innerHTML = integrations.map(ig => {
    const logo = getIntegrationLogo(ig.id);
    const statusClass = ig.configured ? 'connected' : 'disconnected';
    const statusLabel = ig.configured ? 'Connected' : 'Not configured';
    const statusDot = ig.configured ? '●' : '○';
    const selectedClass = expandedIntegration === ig.id ? ' expanded' : '';
    return `
      <div class="integration-card${selectedClass}" data-id="${ig.id}" onclick="toggleIntegration('${ig.id}')">
        <div class="integration-logo">${logo}</div>
        <div class="integration-name">${escapeHtml(ig.name)}</div>
        <span class="integration-status ${statusClass}">${statusDot} ${statusLabel}</span>
        ${ig.auth_mode ? `<div class="integration-auth-mode">${escapeHtml(ig.auth_mode)}</div>` : ''}
      </div>`;
  }).join('');

  if (!expandedIntegration) { detailPanel.innerHTML = ''; return; }
  const ig = integrations.find(i => i.id === expandedIntegration);
  if (!ig) { detailPanel.innerHTML = ''; return; }

  const logo = getIntegrationLogo(ig.id);
  const statusClass = ig.configured ? 'connected' : 'disconnected';
  const statusLabel = ig.configured ? 'Connected' : 'Not configured';
  const statusDot = ig.configured ? '●' : '○';

  detailPanel.innerHTML = `
    <div class="integration-detail-panel">
      <div class="integration-detail-header">
        <div class="integration-logo">${logo}</div>
        <div>
          <div class="integration-name">${escapeHtml(ig.name)}</div>
          <div style="display:flex;gap:8px;align-items:center;margin-top:4px;">
            <span class="integration-status ${statusClass}">${statusDot} ${statusLabel}</span>
            ${ig.auth_mode ? `<span class="integration-auth-mode">${escapeHtml(ig.auth_mode)}</span>` : ''}
          </div>
        </div>
        <button class="integration-detail-close" onclick="toggleIntegration('${ig.id}')" title="Close">&times;</button>
      </div>
      ${ig.active_models && Object.keys(ig.active_models).length ? `<div class="integration-active-models">${Object.entries(ig.active_models).map(([label, model]) => `<div class="integration-active-model"><span class="model-label">${escapeHtml(label)}</span><span class="model-value">${escapeHtml(model)}</span></div>`).join('')}</div>` : ''}
      <div class="integration-tabs">
        <button class="integration-tab${activeIntegrationTab === 'permissions' ? ' active' : ''}" data-tab="permissions" onclick="setIntegrationTab('permissions')">Permissions <span class="tab-count">${ig.permissions.length}</span></button>
        <button class="integration-tab${activeIntegrationTab === 'tools' ? ' active' : ''}" data-tab="tools" onclick="setIntegrationTab('tools')">Tools <span class="tab-count">${(ig.tools || []).length}</span></button>
      </div>
      <div class="integration-tab-panel" data-tab-panel="permissions"${activeIntegrationTab === 'permissions' ? '' : ' hidden'}>
      <table class="permissions-table">
        <thead>
          <tr>
            <th>Scope / Permission</th>
            <th>Description</th>
            <th>Status</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          ${ig.permissions.map(p => {
            let statusHtml;
            if (p.extra) {
              statusHtml = '<span class="perm-status warning"><span class="perm-status-icon">⚠</span> Extra</span>';
            } else if (p.granted === true) {
              statusHtml = '<span class="perm-status granted"><span class="perm-status-icon">✓</span> Granted</span>';
            } else if (p.granted === false) {
              statusHtml = '<span class="perm-status denied"><span class="perm-status-icon">✗</span> Missing</span>';
            } else {
              statusHtml = '<span class="perm-status unknown">—</span>';
            }
            const badgeClass = p.extra ? 'extra' : (p.required ? 'required' : 'optional');
            const badgeLabel = p.extra ? 'Extra' : (p.required ? 'Required' : 'Optional');
            return `
            <tr>
              <td class="scope-name">${escapeHtml(p.scope)}</td>
              <td class="scope-desc">${p.description ? escapeHtml(p.description) : '<span style="color:var(--text-muted);font-style:italic">Not used by arbetern</span>'}</td>
              <td>${statusHtml}</td>
              <td><span class="perm-badge ${badgeClass}">${badgeLabel}</span></td>
            </tr>`;
          }).join('')}
        </tbody>
      </table>
      </div>
      <div class="integration-tab-panel" data-tab-panel="tools"${activeIntegrationTab === 'tools' ? '' : ' hidden'}>
        ${renderIntegrationTools(ig.tools)}
      </div>
    </div>`;
}

// Tools are listed one per row; the description column appears only when the
// integration exposes at least one description.
function renderIntegrationTools(tools) {
  const list = (tools || []).map(t => (typeof t === 'string' ? { name: t } : t));
  if (!list.length) return '<div class="integration-tools-empty">No tools are exposed by this integration.</div>';
  const withDesc = list.some(t => t.description);
  const rows = list.map(t => `
            <tr>
              <td class="scope-name">${escapeHtml(t.name)}</td>
              ${withDesc ? `<td class="scope-desc">${escapeHtml(t.description || '')}</td>` : ''}
            </tr>`).join('');
  return `
      <table class="permissions-table tools-table">
        <thead>
          <tr>
            <th>Tool</th>
            ${withDesc ? '<th>Description</th>' : ''}
          </tr>
        </thead>
        <tbody>${rows}</tbody>
      </table>`;
}

function toggleIntegration(id) {
  const opening = expandedIntegration !== id;
  expandedIntegration = opening ? id : null;
  if (opening) activeIntegrationTab = 'permissions';
  renderIntegrations(integrationsData || []);
}

function setIntegrationTab(tab) {
  activeIntegrationTab = tab;
  document.querySelectorAll('.integration-tab').forEach(b => b.classList.toggle('active', b.dataset.tab === tab));
  document.querySelectorAll('.integration-tab-panel').forEach(p => { p.hidden = p.dataset.tabPanel !== tab; });
}

/* Editor panel shared by skills and connectors */
let formState = null;

function openForm({ title, subtitle, html, saveLabel, onSave }) {
  document.getElementById('form-title').textContent = title;
  document.getElementById('form-subtitle').textContent = subtitle || '';
  document.getElementById('form-body').innerHTML = html;
  document.getElementById('form-error').textContent = '';
  const save = document.getElementById('form-save');
  save.textContent = saveLabel || 'Save';
  save.disabled = false;
  formState = { onSave };
  document.getElementById('form-overlay').classList.add('active');
  const first = document.querySelector('#form-body input, #form-body textarea');
  if (first) setTimeout(() => first.focus(), 120);
}

function closeForm() {
  document.getElementById('form-overlay').classList.remove('active');
  formState = null;
}

async function submitForm() {
  if (!formState) return;
  const save = document.getElementById('form-save');
  const errEl = document.getElementById('form-error');
  save.disabled = true;
  errEl.textContent = '';
  try {
    await formState.onSave();
    closeForm();
  } catch (err) {
    errEl.textContent = err && err.message ? err.message : String(err);
    save.disabled = false;
  }
}

document.getElementById('form-save').addEventListener('click', submitForm);
document.getElementById('form-cancel').addEventListener('click', closeForm);
document.getElementById('form-close').addEventListener('click', closeForm);
document.getElementById('form-overlay').addEventListener('click', e => {
  if (e.target === document.getElementById('form-overlay')) closeForm();
});
document.getElementById('form-body').addEventListener('submit', e => { e.preventDefault(); submitForm(); });
document.getElementById('form-body').addEventListener('click', e => {
  const chip = e.target.closest('.chip-select .chip');
  if (chip) { chip.classList.toggle('on'); return; }
  const del = e.target.closest('.kv-del');
  if (del) { del.closest('.kv-row').remove(); return; }
  if (e.target.closest('#f-add-header')) document.getElementById('f-headers').insertAdjacentHTML('beforeend', headerRowHtml('', ''));
});

function formField(label, inputHtml, hint) {
  return `<div class="form-field"><div class="form-label">${label}</div>${inputHtml}${hint ? `<div class="form-hint">${hint}</div>` : ''}</div>`;
}

function formValue(id) {
  const el = document.getElementById(id);
  return el ? el.value.trim() : '';
}

function agentChipsHtml(selected) {
  return `<div class="chip-select" id="f-agents">${agentsData.map(a =>
    `<button type="button" class="chip${selected.includes(a.id) ? ' on' : ''}" data-id="${a.id}">${miniAvatar(a.id)}${escapeHtml(a.name)}</button>`).join('')}</div>`;
}

function selectedAgents() {
  return [...document.querySelectorAll('#f-agents .chip.on')].map(b => b.dataset.id);
}

async function apiSend(url, method, body) {
  const init = { method };
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' };
    init.body = JSON.stringify(body);
  }
  const r = await fetch(url, init);
  if (!r.ok) {
    const text = (await r.text()).trim();
    throw new Error(text || ('HTTP ' + r.status));
  }
  return r.status === 204 ? null : r.json();
}

function scopeChips(agents) {
  return !agents || !agents.length ? '<span class="tag">All agents</span>' : agents.map(miniAvatar).join('');
}

/* Chats */
function chatAgents() { return agentsData.filter(a => a.chat_enabled); }

async function loadChats() {
  const agents = chatAgents();
  if (!agents.length) { renderChatsPage(); return; }
  if (!chatsAgent || !agents.some(a => a.id === chatsAgent)) chatsAgent = agents[0].id;
  const agent = chatsAgent;
  if (chatsLoading) return;
  if (chatsList && chatsList.agent === agent && recentlyFetched('chats:' + agent)) return;
  chatsLoading = true;
  try {
    const list = await apiListConversations(agent);
    if (chatsAgent === agent) chatsList = { agent, list, error: null };
  } catch (err) {
    if (chatsAgent === agent) chatsList = { agent, list: null, error: err };
  }
  chatsLoading = false;
  renderChatsPage();
}

function renderChatsPage() {
  const el = document.getElementById('chat-list');
  const pills = document.getElementById('chat-agent-pills');
  const newBtn = document.getElementById('chat-new-btn');
  if (!agentsData.length) return;
  const agents = chatAgents();
  if (!agents.length) {
    pills.innerHTML = '';
    newBtn.hidden = true;
    el.innerHTML = emptyHtml('No agent has chat enabled. Turn on chat for an agent in its config to start a conversation here.', true);
    return;
  }
  newBtn.hidden = false;
  if (!chatsAgent || !agents.some(a => a.id === chatsAgent)) chatsAgent = agents[0].id;
  pills.innerHTML = agents.length > 1
    ? agents.map(a => `<button class="pill${a.id === chatsAgent ? ' active' : ''}" data-agent="${a.id}">${escapeHtml(a.name)}</button>`).join('')
    : '';
  if (!chatsList || chatsList.agent !== chatsAgent) {
    el.innerHTML = emptyHtml('Loading conversations…', true);
    if (!chatsLoading) loadChats();
    return;
  }
  if (chatsList.error) {
    el.innerHTML = emptyHtml(chatsList.error.status === 403 ? 'You don’t have access to this agent’s chat.' : 'Failed to load conversations.', true);
    return;
  }
  const list = chatsList.list || [];
  if (!list.length) {
    el.innerHTML = emptyHtml(`No conversations with ${agentLabel(chatsAgent)} yet. Start one with New chat.`, true);
    return;
  }
  el.innerHTML = `<table class="data-table"><thead><tr><th>Conversation</th><th>Agent</th><th class="n">Messages</th><th>Last activity</th><th></th></tr></thead><tbody>${
    list.map(c => `<tr>
      <td><a href="/ui/${encodeURIComponent(chatsAgent)}/chat" onclick="event.preventDefault();openFullChat('${chatsAgent}','${c.id}')">${escapeHtml(c.title || 'New chat')}</a><span class="sub">started ${timeAgo(c.created_at)}</span></td>
      <td>${agentChip(chatsAgent)}</td>
      <td class="n">${fmtInt(c.message_count)}</td>
      <td class="muted" title="${escapeHtml(new Date(c.updated_at).toLocaleString())}">${timeAgo(c.updated_at)}</td>
      <td><div class="actions">
        <button class="btn-mini" type="button" onclick="openFullChat('${chatsAgent}','${c.id}')">Open</button>
        <button class="btn-mini" type="button" onclick="renameChatFromList('${c.id}')">Rename</button>
        <button class="btn-mini danger" type="button" onclick="deleteChatFromList('${c.id}')">Delete</button>
      </div></td>
    </tr>`).join('')}</tbody></table>`;
}

document.getElementById('chat-agent-pills').addEventListener('click', e => {
  const b = e.target.closest('.pill');
  if (!b) return;
  chatsAgent = b.dataset.agent;
  chatsList = null;
  renderChatsPage();
});

document.getElementById('chat-new-btn').addEventListener('click', async () => {
  if (!chatsAgent) return;
  try {
    const conv = await apiCreateConversation(chatsAgent);
    openFullChat(chatsAgent, conv.id);
  } catch (err) {
    alert(err && err.status === 403 ? 'You don’t have access to this agent’s chat.' : 'Failed to start a chat: ' + err);
  }
});

async function renameChatFromList(id) {
  const c = chatsList && chatsList.list ? chatsList.list.find(x => x.id === id) : null;
  const title = prompt('Rename conversation', c ? c.title || '' : '');
  if (title == null || !title.trim()) return;
  try {
    await apiRenameConversation(chatsAgent, id, title.trim());
  } catch (err) {
    alert('Failed to rename conversation: ' + err);
  }
  chatsList = null;
  loadChats();
}

async function deleteChatFromList(id) {
  if (!confirm('Delete this conversation? This removes its history for everyone.')) return;
  try {
    await apiDeleteConversation(chatsAgent, id);
  } catch (err) {
    alert('Failed to delete conversation: ' + err);
  }
  chatsList = null;
  loadChats();
}

/* Skills */
async function loadSkills() {
  if (recentlyFetched('skills')) return;
  try {
    skillsData = { list: await fetchJSON('/api/skills'), error: null };
  } catch (err) {
    skillsData = { list: null, error: err };
  }
  renderSkillsPage();
}

function firstLine(text, max) {
  const line = String(text || '').split('\n').map(l => l.trim().replace(/^[-•*#\s]+/, '')).find(Boolean) || '';
  return line.length > max ? line.slice(0, max - 1) + '…' : line;
}

function skillApplies(s, agent) {
  return agent === 'all' || !s.agents || !s.agents.length || s.agents.includes(agent);
}

function skillCard(s) {
  const custom = s.kind === 'custom';
  const desc = s.description || (custom ? firstLine(s.instructions, 120) : '');
  const when = s.updated_at || s.created_at;
  const sub = custom ? (s.created_by ? 'by ' + escapeHtml(s.created_by) : 'custom skill') : escapeHtml(s.source || 'prompt file');
  return `<article class="card${!custom || s.enabled ? '' : ' off'}">
      <div class="card-head">
        <div><div class="card-name" title="${escapeHtml(s.name)}">${escapeHtml(s.name)}</div><div class="card-sub">${sub}</div></div>
        <div class="card-scope">${scopeChips(s.agents)}</div>
      </div>
      ${desc ? `<div class="card-desc">${escapeHtml(desc)}</div>` : ''}
      <details><summary>Instructions</summary><pre>${escapeHtml(s.instructions)}</pre></details>
      <div class="card-foot">
        <span class="tag ${custom ? (s.enabled ? 'workflow' : '') : 'gitops'}">${custom ? (s.enabled ? 'enabled' : 'disabled') : 'built-in'}</span>
        ${custom && when ? `<span>updated ${timeAgo(when)}</span>` : ''}
        ${custom ? `<div class="actions">
          <button class="btn-mini" type="button" onclick="toggleSkill('${s.id}', ${!s.enabled})">${s.enabled ? 'Disable' : 'Enable'}</button>
          <button class="btn-mini" type="button" onclick="editSkill('${s.id}')">Edit</button>
          <button class="btn-mini danger" type="button" onclick="deleteSkill('${s.id}')">Delete</button>
        </div>` : ''}
      </div>
    </article>`;
}

function renderSkillsPage() {
  const customEl = document.getElementById('skills-custom');
  const builtinEl = document.getElementById('skills-builtin');
  const filterEl = document.getElementById('skill-filter');
  filterEl.innerHTML = agentsData.length > 1
    ? `<button class="pill${skillFilter === 'all' ? ' active' : ''}" data-agent="all">All agents</button>` +
      agentsData.map(a => `<button class="pill${skillFilter === a.id ? ' active' : ''}" data-agent="${a.id}">${escapeHtml(a.name)}</button>`).join('')
    : '';
  if (!skillsData) return;
  if (skillsData.error) {
    customEl.innerHTML = emptyHtml('Failed to load skills.', true);
    builtinEl.innerHTML = '';
    return;
  }
  const list = skillsData.list.filter(s => skillApplies(s, skillFilter));
  const custom = list.filter(s => s.kind === 'custom');
  const builtin = list.filter(s => s.kind !== 'custom');
  document.getElementById('skills-custom-meta').textContent = custom.length ? `${custom.length} · ${custom.filter(s => s.enabled).length} enabled` : '';
  document.getElementById('skills-builtin-meta').textContent = builtin.length ? String(builtin.length) : '';
  customEl.innerHTML = custom.length ? custom.map(skillCard).join('') : emptyHtml('No custom skills yet. Write one to give the agents an extra instruction block.', true);
  builtinEl.innerHTML = builtin.length ? builtin.map(skillCard).join('') : emptyHtml('No built-in skills apply here.');
}

document.getElementById('skill-filter').addEventListener('click', e => {
  const b = e.target.closest('.pill');
  if (!b) return;
  skillFilter = b.dataset.agent;
  renderSkillsPage();
});
document.getElementById('skill-new-btn').addEventListener('click', () => skillForm(null));

function skillForm(s) {
  const isNew = !s;
  openForm({
    title: isNew ? 'New skill' : 'Edit skill',
    subtitle: isNew ? 'Appended to the system prompt of the agents you pick' : s.name,
    saveLabel: isNew ? 'Create skill' : 'Save changes',
    html: formField('Name', `<input class="form-input" id="f-name" maxlength="80" value="${escapeHtml(s ? s.name : '')}" placeholder="Incident write-ups">`)
      + formField('Description', `<input class="form-input" id="f-desc" maxlength="240" value="${escapeHtml(s ? s.description || '' : '')}" placeholder="One line on when this applies">`)
      + formField('Instructions', `<textarea class="form-textarea" id="f-instructions" placeholder="Write the instruction block exactly as the agent should read it.">${escapeHtml(s ? s.instructions : '')}</textarea>`, 'Plain text or Markdown. Slack replies still follow the agent’s formatting rules.')
      + formField('Agents', agentChipsHtml(s ? s.agents || [] : []), 'Leave every agent unselected to apply the skill to all of them.')
      + `<label class="check-row"><input type="checkbox" id="f-enabled"${!s || s.enabled ? ' checked' : ''}> Enabled</label>`,
    onSave: async () => {
      const body = {
        name: formValue('f-name'),
        description: formValue('f-desc'),
        instructions: document.getElementById('f-instructions').value.trim(),
        agents: selectedAgents(),
        enabled: document.getElementById('f-enabled').checked,
      };
      if (isNew) await apiSend('/api/skills', 'POST', body);
      else await apiSend(`/api/skills/${encodeURIComponent(s.id)}`, 'PATCH', body);
      delete lastFetched.skills;
      await loadSkills();
    },
  });
}

function editSkill(id) {
  const s = skillsData && skillsData.list ? skillsData.list.find(x => x.id === id) : null;
  if (s) skillForm(s);
}

async function toggleSkill(id, enabled) {
  try {
    await apiSend(`/api/skills/${encodeURIComponent(id)}`, 'PATCH', { enabled });
  } catch (err) {
    alert('Failed to update skill: ' + err.message);
  }
  delete lastFetched.skills;
  await loadSkills();
}

async function deleteSkill(id) {
  if (!confirm('Delete this skill? Agents stop following it immediately.')) return;
  try {
    await apiSend(`/api/skills/${encodeURIComponent(id)}`, 'DELETE');
  } catch (err) {
    alert('Failed to delete skill: ' + err.message);
  }
  delete lastFetched.skills;
  await loadSkills();
}

/* MCP connectors */
async function loadMCP() {
  if (recentlyFetched('mcp')) return;
  try {
    mcpData = { list: await fetchJSON('/api/mcp'), error: null };
  } catch (err) {
    mcpData = { list: null, error: err };
  }
  renderMCPPage();
  renderFleet();
}

function connectorHost(u) {
  try { return new URL(u).host; } catch (e) { return u; }
}

function connectorCheck(c) {
  if (c.last_error) return ['failed', 'check failed'];
  if (c.last_check) return ['ok', plural((c.tools || []).length, 'tool')];
  return ['untested', 'not tested'];
}

function connectorCard(c) {
  const [cls, label] = connectorCheck(c);
  const tools = c.tools || [];
  const headers = Object.keys(c.headers || {});
  const server = c.server_name ? ' · ' + escapeHtml(c.server_name + (c.server_version ? ' ' + c.server_version : '')) : '';
  return `<article class="card${c.enabled ? '' : ' off'}">
      <div class="card-head">
        <div><div class="card-name" title="${escapeHtml(c.name)}">${escapeHtml(c.name)}</div><div class="card-sub" title="${escapeHtml(c.url)}">${escapeHtml(connectorHost(c.url))}${server}</div></div>
        <div class="card-scope"><span class="status-pill ${c.enabled ? 'ok' : 'paused'}">${c.enabled ? 'enabled' : 'disabled'}</span><span class="status-pill ${cls}" title="${escapeHtml(c.last_error || '')}">${label}</span></div>
      </div>
      ${c.description ? `<div class="card-desc">${escapeHtml(c.description)}</div>` : ''}
      ${c.last_error ? `<div class="card-note">${escapeHtml(c.last_error)}</div>` : ''}
      <div class="card-scope" style="justify-content:flex-start"><span style="font-size:12px;color:var(--text-muted)">Agents</span>${scopeChips(c.agents)}</div>
      ${tools.length ? `<details><summary>${plural(tools.length, 'tool')}</summary><div class="tool-chips" style="margin-top:8px">${tools.map(t => `<code class="tool-chip" title="${escapeHtml(t.description || '')}">${escapeHtml(t.name)}</code>`).join('')}</div></details>` : ''}
      <div class="card-foot">
        <span>${headers.length ? plural(headers.length, 'header') + ' · ' : ''}${c.last_check ? 'checked ' + timeAgo(c.last_check) : 'never checked'}</span>
        ${canManageMCP() ? `<div class="actions">
          <button class="btn-mini" type="button" onclick="testConnector('${c.id}', this)">Test</button>
          <button class="btn-mini" type="button" onclick="toggleConnector('${c.id}', ${!c.enabled})">${c.enabled ? 'Disable' : 'Enable'}</button>
          <button class="btn-mini" type="button" onclick="editConnector('${c.id}')">Edit</button>
          <button class="btn-mini danger" type="button" onclick="deleteConnector('${c.id}')">Delete</button>
        </div>` : ''}
      </div>
    </article>`;
}

// canManageMCP reflects the mcp_admin flag of the identity endpoint; until the
// identity is known the actions stay visible and the API is the arbiter.
function canManageMCP() {
  return !(identityData && identityData.mcp_admin === false);
}

function renderMCPPage() {
  const el = document.getElementById('mcp-list');
  const manage = canManageMCP();
  document.getElementById('mcp-new-btn').hidden = !manage;
  const note = document.getElementById('mcp-readonly-note');
  if (note) note.hidden = manage;
  if (!mcpData) return;
  if (mcpData.error) { el.innerHTML = emptyHtml('Failed to load connectors.', true); return; }
  const list = mcpData.list || [];
  el.innerHTML = list.length
    ? list.map(connectorCard).join('')
    : emptyHtml(manage ? 'No connectors yet. Add an MCP server to give the agents its tools.' : 'No connectors yet.', true);
}

document.getElementById('mcp-new-btn').addEventListener('click', () => connectorForm(null));

function headerRowHtml(k, v) {
  return `<div class="kv-row"><input class="form-input kv-key" placeholder="Header name" value="${escapeHtml(k)}"><input class="form-input kv-val" placeholder="Value or \${ENV_VAR}" value="${escapeHtml(v)}"><button type="button" class="btn-mini danger kv-del">Remove</button></div>`;
}

function collectHeaders() {
  const out = {};
  document.querySelectorAll('#f-headers .kv-row').forEach(row => {
    const k = row.querySelector('.kv-key').value.trim();
    const v = row.querySelector('.kv-val').value.trim();
    if (k && v) out[k] = v;
  });
  return out;
}

function connectorForm(c) {
  const isNew = !c;
  const headers = Object.entries((c && c.headers) || {});
  openForm({
    title: isNew ? 'Add connector' : 'Edit connector',
    subtitle: isNew ? 'A Model Context Protocol server reachable over HTTP' : c.name,
    saveLabel: isNew ? 'Add and test' : 'Save changes',
    html: formField('Name', `<input class="form-input" id="f-name" maxlength="80" value="${escapeHtml(c ? c.name : '')}" placeholder="Internal docs search">`)
      + formField('Description', `<input class="form-input" id="f-desc" maxlength="240" value="${escapeHtml(c ? c.description || '' : '')}" placeholder="What this server offers">`)
      + formField('Server URL', `<input class="form-input" id="f-url" value="${escapeHtml(c ? c.url : '')}" placeholder="https://mcp.example.com/mcp">`, 'Streamable HTTP endpoint, reached from where this app runs.')
      + formField('Headers', `<div class="kv-rows" id="f-headers">${headers.map(([k, v]) => headerRowHtml(k, v)).join('')}</div><div><button type="button" class="btn-mini" id="f-add-header">Add header</button></div>`, 'Sent on every request, for example Authorization. Write a value as \${TOKEN_ENV_VAR} to read it from the environment instead of storing it here. Saved values are masked.')
      + formField('Agents', agentChipsHtml(c ? c.agents || [] : []), 'Leave every agent unselected to expose the tools to all of them.')
      + `<label class="check-row"><input type="checkbox" id="f-enabled"${!c || c.enabled ? ' checked' : ''}> Enabled</label>`,
    onSave: async () => {
      const body = {
        name: formValue('f-name'),
        description: formValue('f-desc'),
        url: formValue('f-url'),
        headers: collectHeaders(),
        agents: selectedAgents(),
        enabled: document.getElementById('f-enabled').checked,
      };
      const saved = isNew ? await apiSend('/api/mcp', 'POST', body) : await apiSend(`/api/mcp/${encodeURIComponent(c.id)}`, 'PATCH', body);
      delete lastFetched.mcp;
      await loadMCP();
      if (saved && saved.id && (isNew || !saved.last_check)) testConnector(saved.id, null);
    },
  });
}

function editConnector(id) {
  const c = mcpData && mcpData.list ? mcpData.list.find(x => x.id === id) : null;
  if (c) connectorForm(c);
}

async function toggleConnector(id, enabled) {
  try {
    await apiSend(`/api/mcp/${encodeURIComponent(id)}`, 'PATCH', { enabled });
  } catch (err) {
    alert('Failed to update connector: ' + err.message);
  }
  delete lastFetched.mcp;
  await loadMCP();
}

async function deleteConnector(id) {
  if (!confirm('Delete this connector? Its tools disappear from the agents immediately.')) return;
  try {
    await apiSend(`/api/mcp/${encodeURIComponent(id)}`, 'DELETE');
  } catch (err) {
    alert('Failed to delete connector: ' + err.message);
  }
  delete lastFetched.mcp;
  await loadMCP();
}

async function testConnector(id, btn) {
  if (btn) { btn.disabled = true; btn.textContent = 'Testing…'; }
  try {
    const c = await apiSend(`/api/mcp/${encodeURIComponent(id)}/test`, 'POST');
    if (mcpData && mcpData.list) {
      const i = mcpData.list.findIndex(x => x.id === id);
      if (i >= 0) mcpData.list[i] = c;
    }
  } catch (err) {
    alert('Test failed: ' + err.message);
  }
  renderMCPPage();
  renderFleet();
}

/* Identity */
let identityData = null;

function initialsOf(name) {
  const parts = String(name || '').trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return '?';
  return (parts[0][0] + (parts.length > 1 ? parts[parts.length - 1][0] : '')).toUpperCase();
}

function avatarHtml(name, url, muted) {
  const style = !muted && name ? ` style="background:${hashColor(name)}"` : '';
  const imageUrl = safeImageUrl(url);
  const img = imageUrl ? `<img src="${escapeHtml(imageUrl)}" alt="" onerror="this.remove()">` : '';
  return `<span class="user-avatar${muted ? ' muted' : ''}"${style}>${escapeHtml(muted ? '?' : initialsOf(name))}${img}</span>`;
}

function idRows(rows) {
  const items = rows.filter(r => r[1]);
  if (!items.length) return '';
  return `<dl class="id-row">${items.map(([k, v, cls]) => { const link = cls === 'link' ? safeExternalUrl(v) : ''; return `<dt>${escapeHtml(k)}</dt><dd class="${cls || ''}" title="${escapeHtml(v)}">${link ? `<a href="${escapeHtml(link)}" target="_blank" rel="noopener">${escapeHtml(v.replace(/^https?:\/\//, ''))}</a>` : escapeHtml(v)}</dd>`; }).join('')}</dl>`;
}

function renderIdentity() {
  const btn = document.getElementById('user-button');
  const pop = document.getElementById('user-pop');
  const me = identityData;
  if (!me) return;
  if (me.anonymous || me.error) {
    btn.innerHTML = avatarHtml('', '', true) + `<span class="user-name">${me.error ? 'Identity unavailable' : 'Not signed in'}</span>`;
    pop.innerHTML = `<div class="id-head">${avatarHtml('', '', true)}<div><div class="id-title">${me.error ? 'Identity unavailable' : 'Not signed in'}</div><div class="id-sub">${me.error ? 'The identity endpoint did not respond' : 'No identity from the sign-in proxy'}</div></div></div>
      <div class="id-section"><div class="id-note">When the console runs behind the SSO proxy, your Slack profile and Atlassian account appear here.</div></div>`;
    return;
  }
  const s = me.slack || {};
  const displayName = s.real_name || s.display_name || (me.email || '').split('@')[0];
  btn.innerHTML = avatarHtml(displayName, s.avatar, false) + `<span class="user-name">${escapeHtml(displayName)}</span>`;

  const slackSection = me.slack
    ? idRows([
        ['Name', s.real_name],
        ['Display name', s.display_name ? (s.handle ? `${s.display_name} (@${s.handle})` : s.display_name) : (s.handle ? '@' + s.handle : '')],
        ['Title', s.title],
        ['Time zone', s.timezone],
        ['User ID', s.id, 'mono'],
      ])
    : '<div class="id-note">No Slack account matches this email.</div>';

  let atlassianSection;
  if (!me.atlassian_connected) atlassianSection = '<div class="id-note">Atlassian is not connected.</div>';
  else if (!me.atlassian) atlassianSection = '<div class="id-note">No Atlassian account matches this email.</div>';
  else {
    const a = me.atlassian;
    atlassianSection = idRows([
      ['Name', a.display_name],
      ['Email', a.email],
      ['Account ID', a.account_id, 'mono'],
      ['Site', a.site, 'link'],
    ]);
  }

  pop.innerHTML = `<div class="id-head">${avatarHtml(displayName, s.avatar, false)}<div><div class="id-title">${escapeHtml(displayName)}</div><div class="id-sub" title="${escapeHtml(me.email)}">${escapeHtml(me.email)}</div></div></div>
    <div class="id-section"><h3>${INTEGRATION_LOGOS.slack}Slack<span class="tag ${me.slack ? 'slack' : ''}">${me.slack ? 'matched' : 'not found'}</span></h3>${slackSection}</div>
    <div class="id-section"><h3>${INTEGRATION_LOGOS.jira}Atlassian<span class="tag">${!me.atlassian_connected ? 'not connected' : me.atlassian ? 'matched' : 'not found'}</span></h3>${atlassianSection}</div>
    <div class="id-foot">Resolved ${me.resolved_at ? timeAgo(me.resolved_at) : 'just now'} · identity comes from the sign-in proxy</div>`;
}

function setIdentityOpen(open) {
  const btn = document.getElementById('user-button');
  const pop = document.getElementById('user-pop');
  pop.hidden = !open;
  btn.setAttribute('aria-expanded', String(open));
}

document.getElementById('user-button').addEventListener('click', e => {
  e.stopPropagation();
  setIdentityOpen(document.getElementById('user-pop').hidden);
});
document.addEventListener('click', e => {
  if (!e.target.closest('#user-menu')) setIdentityOpen(false);
});

async function loadIdentity() {
  try {
    identityData = await fetchJSON('/api/me');
  } catch (err) {
    identityData = { anonymous: true, error: true };
  }
  renderIdentity();
  renderMCPPage();
}

/* Branding */
(function loadLogo() {
  const img = new Image();
  img.onload = function () {
    const el = document.getElementById('logo-icon');
    el.textContent = '';
    el.classList.add('has-logo');
    el.appendChild(img);
  };
  img.src = '/ui/logo.png';
})();

(async function loadSettings() {
  try {
    const data = await fetchJSON('/api/settings');
    if (data.header) {
      appTitle = data.header;
      document.getElementById('header-title').textContent = data.header;
      if (currentPage) document.title = `${appTitle} — ${PAGE_TITLES[currentPage]}`;
    }
  } catch (e) {}
})();

/* Boot — every dataset the console renders is fetched up front and in parallel,
   so each page and widget draws from memory instead of its own round trip. */
function prefetchAll() {
  return Promise.allSettled([
    loadIntegrations(), loadWorkflows(), loadDashboards(), loadChanges(), loadSessions(),
    loadBilling(), loadSkills(), loadMCP(), loadGitops('workflows'), loadGitops('dashboards'),
  ]);
}

applyRoute();
loadIdentity();
loadAgents().then(loadChats);
prefetchAll();
setInterval(() => {
  if (document.visibilityState !== 'visible' || chatFull) return;
  if (currentPage === 'overview' || currentPage === 'billing') loadPage(currentPage);
}, REFRESH_MS);
