// Externalized UI strings (i18n-ready). PT-only in the MVP; adding EN later is swapping this
// object behind a locale switch, not retouching every component. No hardcoded copy in components.
export const t = {
	brand: 'rara',
	nav: {
		overview: 'Visão geral',
		pipeline: 'Pipeline',
		quarantine: 'Quarentena',
		distillations: 'Distillations',
		secTrain: 'Treinar',
		curation: 'Curadoria',
		sources: 'Fontes & Flows',
		providers: 'Providers & Roteamento',
		secSystem: 'Sistema',
		audit: 'Auditoria',
		settings: 'Configurações'
	},
	topbar: {
		search: 'Buscar ou comandar…',
		clean: 'Clean',
		dark: 'Dark'
	},
	status: { online: 'VPC Oracle · online', offline: 'core inacessível' },
	overview: {
		title: 'Visão geral',
		kpiFlows: 'Flows',
		kpiProviders: 'Providers',
		kpiEnabled: 'Providers ativos',
		flowsPanel: 'Flows configurados',
		providersPanel: 'Providers registrados',
		loading: 'Carregando do core ao vivo…',
		error: 'Não foi possível ler a superfície do core.',
		empty: 'Nada seedado ainda.'
	}
};
